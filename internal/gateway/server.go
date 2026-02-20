package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
	"github.com/clock-p/clockbridge/internal/shared"
	"github.com/gorilla/websocket"
)

type Server struct {
	agentTokenSet map[string]struct{}

	mu          syncMap
	agentByUUID map[string]*agentConn

	upgrader websocket.Upgrader

	maxStreamsPerAgent int
	maxBodyBytes       int64
	streamIdleTimeout  time.Duration
	heartbeatTimeout   time.Duration
	firstRespTimeout   time.Duration
	wsOpenTimeout      time.Duration
	wsReadLimitBytes   int64
	nextReqID          atomic.Uint64
}

// 简单 map 封装，避免引入 sync.Map 但又显式上锁。
type syncMap struct {
	mu sync.Mutex
}

func (m *syncMap) lock()   { m.mu.Lock() }
func (m *syncMap) unlock() { m.mu.Unlock() }

func NewServerFromEnv() *Server {
	tokens := strings.Split(os.Getenv("HTTPS_PROXY_AGENT_TOKENS"), ",")
	set := make(map[string]struct{})
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		set[t] = struct{}{}
	}

	maxStreams := envInt("HTTPS_PROXY_MAX_STREAMS_PER_AGENT", 0)
	maxBody := envInt64("HTTPS_PROXY_MAX_BODY_BYTES", 512<<20)
	streamIdle := envDuration("HTTPS_PROXY_STREAM_IDLE_TIMEOUT", 5*time.Minute)
	heartbeatTimeout := envDuration("HTTPS_PROXY_HEARTBEAT_TIMEOUT", 60*time.Second)
	firstRespTimeout := envDuration("HTTPS_PROXY_FIRST_RESPONSE_TIMEOUT", 30*time.Second)
	wsOpenTimeout := envDuration("HTTPS_PROXY_WS_OPEN_TIMEOUT", 10*time.Second)

	return &Server{
		agentTokenSet: set,
		agentByUUID:   make(map[string]*agentConn),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		maxStreamsPerAgent: maxStreams,
		maxBodyBytes:       maxBody,
		streamIdleTimeout:  streamIdle,
		heartbeatTimeout:   heartbeatTimeout,
		firstRespTimeout:   firstRespTimeout,
		wsOpenTimeout:      wsOpenTimeout,
		wsReadLimitBytes:   shared.WSReadLimitBytes,
	}
}

func MaxHeaderBytesFromEnv() int {
	return envInt("HTTPS_PROXY_MAX_HEADER_BYTES", 1<<20)
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return def
		}
		return d
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	return def
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/status":
		w.Header().Set("Content-Type", "application/json")
		s.mu.lock()
		uuids := make([]string, 0, len(s.agentByUUID))
		for k := range s.agentByUUID {
			uuids = append(uuids, k)
		}
		s.mu.unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    true,
			"count": len(uuids),
			"uuids": uuids,
		})
		return
	case strings.HasPrefix(r.URL.Path, "/register"):
		s.handleRegister(w, r)
		return
	default:
		s.handleClient(w, r)
		return
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Token")
	if len(s.agentTokenSet) > 0 {
		if _, ok := s.agentTokenSet[token]; !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(s.wsReadLimitBytes)

	s.mu.lock()
	if old := s.agentByUUID[uuid]; old != nil {
		s.mu.unlock()
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "uuid already active"), time.Now().Add(2*time.Second))
		_ = ws.Close()
		return
	}
	c := newAgentConn(uuid, ws, s.maxStreamsPerAgent)
	s.agentByUUID[uuid] = c
	s.mu.unlock()

	go c.readLoop()
	go func() {
		<-c.closedCh
		s.mu.lock()
		if s.agentByUUID[uuid] == c {
			delete(s.agentByUUID, uuid)
		}
		s.mu.unlock()
	}()

	if s.heartbeatTimeout > 0 {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			defer func() {
				s.mu.lock()
				if s.agentByUUID[uuid] == c {
					delete(s.agentByUUID, uuid)
				}
				s.mu.unlock()
				_ = ws.Close()
				c.closeWithErr(errors.New("heartbeat timeout"))
			}()

			for {
				select {
				case <-c.closedCh:
					return
				case <-ticker.C:
					last := time.Unix(c.lastPingUnix.Load(), 0)
					if time.Since(last) > s.heartbeatTimeout {
						return
					}
				}
			}
		}()
	}
}

func (s *Server) getAgent(uuid string) *agentConn {
	s.mu.lock()
	defer s.mu.unlock()
	return s.agentByUUID[uuid]
}

func (s *Server) handleClient(w http.ResponseWriter, r *http.Request) {
	reqID := s.nextReqID.Add(1)
	start := time.Now()
	trim := strings.TrimPrefix(r.URL.Path, "/")
	if trim == "" {
		http.NotFound(w, r)
		logAccess(reqID, "", r.Method, r.URL.Path, http.StatusNotFound, 0, start)
		return
	}
	parts := strings.SplitN(trim, "/", 2)
	uuid := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}

	agent := s.getAgent(uuid)
	if agent == nil {
		http.Error(w, "agent offline", http.StatusBadGateway)
		logAccess(reqID, uuid, r.Method, r.URL.Path, http.StatusBadGateway, 0, start)
		return
	}

	suffixPath := "/"
	if rest != "" {
		suffixPath = "/" + rest
	}

	if websocket.IsWebSocketUpgrade(r) {
		logWSOpen(reqID, uuid, r.URL.Path)
		s.handleClientWebSocket(w, r, agent, suffixPath)
		logWSClose(reqID, uuid, r.URL.Path, start)
		return
	}
	lw := &logResponseWriter{ResponseWriter: w}
	s.handleClientHTTP(lw, r, agent, suffixPath)
	logAccess(reqID, uuid, r.Method, r.URL.Path, lw.statusOr(http.StatusOK), lw.bytes, start)
}

func (s *Server) handleClientHTTP(w http.ResponseWriter, r *http.Request, agent *agentConn, suffixPath string) {
	st, err := agent.newStream()
	if err != nil {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer st.closeWithErr(nil)

	if s.maxBodyBytes > 0 {
		if r.ContentLength > s.maxBodyBytes && r.ContentLength != -1 {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			_ = st.send(proto.TRst, []byte("body too large"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	}

	headers := r.Header.Clone()
	stripForwardingIPHeaders(headers)
	trailerKeys := shared.TrailerKeysFromHeader(headers)
	if len(trailerKeys) == 0 && len(r.Trailer) > 0 {
		trailerKeys = shared.HeaderKeys(r.Trailer)
	}
	shared.StripConnectionHeaders(headers)

	reqStart := proto.ReqStart{
		Method:      r.Method,
		Path:        suffixPath,
		RawQuery:    r.URL.RawQuery,
		Host:        r.Host,
		Header:      headers,
		TrailerKeys: trailerKeys,
	}

	if err := st.send(proto.TReqStart, proto.MustJSON(reqStart)); err != nil {
		http.Error(w, "agent send failed", http.StatusBadGateway)
		return
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-r.Context().Done():
			_ = st.send(proto.TRst, []byte("client canceled"))
			st.closeWithErr(errors.New("client canceled"))
		case <-done:
		}
	}()

	// 请求体流式转发
	if r.Body != nil {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				if err2 := st.send(proto.TReqData, buf[:n]); err2 != nil {
					http.Error(w, "agent send failed", http.StatusBadGateway)
					return
				}
			}
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					_ = st.send(proto.TRst, []byte("body too large"))
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				break
			}
		}
		_ = r.Body.Close()
	}

	if len(r.Trailer) > 0 {
		_ = st.send(proto.TReqTrailer, proto.MustJSON(map[string][]string(r.Trailer)))
	}
	_ = st.send(proto.TReqEnd, nil)

	// 响应回传（支持 1xx）
	var resStart proto.ResStart
	for {
		f, err := st.recv(s.firstRespTimeout)
		if err != nil || f.Type != proto.TResStart {
			_ = st.send(proto.TRst, []byte("gateway timeout"))
			http.Error(w, "agent no response", http.StatusBadGateway)
			return
		}
		var rs proto.ResStart
		_ = json.Unmarshal(f.Payload, &rs)
		if rs.Interim || (rs.Status > 0 && rs.Status < 200) {
			writeInterim(w, rs)
			continue
		}
		resStart = rs
		break
	}

	resHeader := http.Header(resStart.Header)
	shared.StripConnectionHeaders(resHeader)
	for k, vv := range resHeader {
		if shared.IsHopByHopHeaderKey(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	setTrailerKeys(w.Header(), resStart.TrailerKeys)

	if resStart.Status <= 0 {
		resStart.Status = http.StatusBadGateway
	}
	w.WriteHeader(resStart.Status)

	flusher, _ := w.(http.Flusher)
	idle := s.streamIdleTimeout
	for {
		f2, err := st.recv(idle)
		if err != nil {
			if errors.Is(err, ErrStreamTimeout) {
				_ = st.send(proto.TRst, []byte("stream idle timeout"))
			}
			return
		}
		switch f2.Type {
		case proto.TResData:
			if _, err := w.Write(f2.Payload); err != nil {
				_ = st.send(proto.TRst, []byte("client write failed"))
				st.closeWithErr(err)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case proto.TResTrailer:
			var tr map[string][]string
			_ = json.Unmarshal(f2.Payload, &tr)
			for k, vv := range tr {
				w.Header()[http.CanonicalHeaderKey(k)] = vv
			}
		case proto.TResEnd:
			return
		case proto.TRst:
			return
		}
	}
}

func (s *Server) handleClientWebSocket(w http.ResponseWriter, r *http.Request, agent *agentConn, suffixPath string) {
	// 先让 Agent 去连上游 WS，成功后再升级客户端，避免客户端升级后挂起。
	st, err := agent.newStream()
	if err != nil {
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
		return
	}
	defer st.closeWithErr(nil)

	wsHeaders := r.Header.Clone()
	stripForwardingIPHeaders(wsHeaders)

	reqStart := proto.ReqStart{
		Method:      r.Method,
		Path:        suffixPath,
		RawQuery:    r.URL.RawQuery,
		Host:        r.Host,
		Header:      wsHeaders,
		IsWebSocket: true,
	}

	if err := st.send(proto.TReqStart, proto.MustJSON(reqStart)); err != nil {
		http.Error(w, "agent send failed", http.StatusBadGateway)
		return
	}

	f, err := st.recv(s.wsOpenTimeout)
	if err != nil {
		_ = st.send(proto.TRst, []byte("agent ws timeout"))
		http.Error(w, "agent ws timeout", http.StatusBadGateway)
		return
	}
	if f.Type == proto.TWsOpenErr {
		var msg proto.WsOpenErr
		_ = json.Unmarshal(f.Payload, &msg)
		if msg.Message != "" {
			http.Error(w, "agent ws open failed: "+msg.Message, http.StatusBadGateway)
		} else {
			http.Error(w, "agent ws open failed", http.StatusBadGateway)
		}
		return
	}
	if f.Type != proto.TWsOpenOK {
		_ = st.send(proto.TRst, []byte("agent ws bad handshake"))
		http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
		return
	}

	var ok proto.WsOpenOK
	_ = json.Unmarshal(f.Payload, &ok)
	respHeader := http.Header{}
	if ok.Subprotocol != "" {
		respHeader.Set("Sec-WebSocket-Protocol", ok.Subprotocol)
	}
	wsClient, err := s.upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		_ = st.send(proto.TWsClose, proto.MustJSON(proto.WsClose{Code: websocket.CloseProtocolError, Reason: "client upgrade failed"}))
		return
	}
	wsClient.SetReadLimit(s.wsReadLimitBytes)
	defer func() { _ = wsClient.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var closeOnce sync.Once
	var closeDone atomic.Bool
	sendCloseToAgent := func(code int, reason string) {
		closeOnce.Do(func() {
			_ = st.send(proto.TWsClose, proto.MustJSON(proto.WsClose{Code: code, Reason: reason}))
			closeDone.Store(true)
		})
	}

	go func() {
		<-ctx.Done()
		if !closeDone.Load() {
			_ = st.send(proto.TRst, []byte("ws client closed"))
		}
		st.closeWithErr(errors.New("ws client closed"))
	}()

	go func() {
		defer cancel()
		for {
			mt, data, err := wsClient.ReadMessage()
			if err != nil {
				if code, reason, ok := shared.CloseFromErr(err); ok {
					sendCloseToAgent(code, reason)
				} else {
					sendCloseToAgent(websocket.CloseGoingAway, "client closed")
				}
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			payload := append([]byte{byte(mt)}, data...)
			if err := st.send(proto.TWsC2A, payload); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		f2, err := st.recv(0)
		if err != nil {
			return
		}
		switch f2.Type {
		case proto.TWsA2C:
			if len(f2.Payload) == 0 {
				continue
			}
			mt := int(f2.Payload[0])
			data := f2.Payload[1:]
			if err := wsClient.WriteMessage(mt, data); err != nil {
				if code, reason, ok := shared.CloseFromErr(err); ok {
					sendCloseToAgent(code, reason)
				} else {
					_ = st.send(proto.TRst, []byte("client write failed"))
				}
				cancel()
				return
			}
		case proto.TWsClose:
			var c proto.WsClose
			_ = json.Unmarshal(f2.Payload, &c)
			code := shared.SanitizeCloseCode(c.Code)
			reason := c.Reason
			closeDone.Store(true)
			_ = wsClient.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(2*time.Second))
			return
		case proto.TRst:
			return
		}
	}
}

func writeInterim(w http.ResponseWriter, rs proto.ResStart) {
	if rs.Status <= 0 {
		return
	}
	h := http.Header(rs.Header)
	shared.StripConnectionHeaders(h)
	for k, vv := range h {
		if shared.IsHopByHopHeaderKey(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rs.Status)
}

func setTrailerKeys(h http.Header, keys []string) {
	if len(keys) == 0 {
		return
	}
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(keys))
	for _, k := range keys {
		ck := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if ck == "" {
			continue
		}
		if _, ok := seen[ck]; ok {
			continue
		}
		seen[ck] = struct{}{}
		uniq = append(uniq, ck)
	}
	if len(uniq) == 0 {
		return
	}
	h.Set("Trailer", strings.Join(uniq, ", "))
}

func stripForwardingIPHeaders(h http.Header) {
	h.Del("X-Forwarded-For")
	h.Del("Forwarded")
}

type logResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *logResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *logResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *logResponseWriter) statusOr(def int) int {
	if w.status == 0 {
		return def
	}
	return w.status
}

func logAccess(id uint64, uuid, method, path string, status int, bytes int64, start time.Time) {
	log.Printf("[clockbridge-gateway] id=%d uuid=%s method=%s path=%s status=%d bytes=%d dur=%s", id, uuid, method, path, status, bytes, time.Since(start))
}

func logWSOpen(id uint64, uuid, path string) {
	log.Printf("[clockbridge-gateway] id=%d uuid=%s ws=open path=%s", id, uuid, path)
}

func logWSClose(id uint64, uuid, path string, start time.Time) {
	log.Printf("[clockbridge-gateway] id=%d uuid=%s ws=close path=%s dur=%s", id, uuid, path, time.Since(start))
}
