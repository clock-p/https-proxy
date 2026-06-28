package gateway

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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

const (
	defaultAgentPingInterval = 20 * time.Second
	minHeartbeatGrace        = 5 * time.Second
)

type Server struct {
	agentTokenSet map[string]struct{}

	mu          syncMap
	agentByUUID map[string]*agentConn

	upgrader websocket.Upgrader

	transparentEnabled       bool
	forceLegacyMode          bool
	transparentAllowUUIDSet  map[string]struct{}
	transparentAllowUUIDs    []string
	transparentAllowPrefixes []string

	maxStreamsPerAgent   int
	maxBodyBytes         int64
	streamIdleTimeout    time.Duration
	heartbeatTimeout     time.Duration
	firstRespTimeout     time.Duration
	wsOpenTimeout        time.Duration
	clientWSPingInterval time.Duration
	clientWSPingDebug    bool
	wsReadLimitBytes     int64
	nextReqID            atomic.Uint64
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
	clientWSPingInterval := envDuration("HTTPS_PROXY_CLIENT_WS_PING_INTERVAL", 0)
	clientWSPingDebug := envBool("HTTPS_PROXY_CLIENT_WS_PING_DEBUG", false)
	pingIntervalHint := envDuration("HTTPS_PROXY_PING_INTERVAL_HINT", defaultAgentPingInterval)
	if pingIntervalHint <= 0 {
		pingIntervalHint = defaultAgentPingInterval
	}
	heartbeatGrace := envDuration("HTTPS_PROXY_HEARTBEAT_GRACE", minHeartbeatGrace)
	if heartbeatGrace < minHeartbeatGrace {
		heartbeatGrace = minHeartbeatGrace
	}
	if heartbeatTimeout > 0 {
		minHeartbeat := pingIntervalHint + heartbeatGrace
		if heartbeatTimeout < minHeartbeat {
			log.Printf(
				"[clockbridge-gateway] adjust heartbeat timeout from %s to %s (ping_interval_hint=%s grace=%s)",
				heartbeatTimeout,
				minHeartbeat,
				pingIntervalHint,
				heartbeatGrace,
			)
			heartbeatTimeout = minHeartbeat
		}
	}

	transparentEnabled := envBool("HTTPS_PROXY_TRANSPARENT_ENABLED", false)
	forceLegacyMode := envBool("HTTPS_PROXY_FORCE_LEGACY_MODE", false)
	transparentAllowUUIDSet, transparentAllowUUIDs := envCommaStringSet("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS")
	transparentAllowPrefixes := envCommaStringSlice("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES")

	if transparentEnabled && len(transparentAllowUUIDs) == 0 && len(transparentAllowPrefixes) == 0 {
		log.Printf("[clockbridge-gateway] transparent mode is enabled but allowlist is empty: all transparent requests will fallback to legacy")
	}
	if forceLegacyMode && transparentEnabled {
		log.Printf("[clockbridge-gateway] force legacy mode is enabled: transparent requests will fallback to legacy")
	}

	return &Server{
		agentTokenSet: set,
		agentByUUID:   make(map[string]*agentConn),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		transparentEnabled:       transparentEnabled,
		forceLegacyMode:          forceLegacyMode,
		transparentAllowUUIDSet:  transparentAllowUUIDSet,
		transparentAllowUUIDs:    transparentAllowUUIDs,
		transparentAllowPrefixes: transparentAllowPrefixes,
		maxStreamsPerAgent:       maxStreams,
		maxBodyBytes:             maxBody,
		streamIdleTimeout:        streamIdle,
		heartbeatTimeout:         heartbeatTimeout,
		firstRespTimeout:         firstRespTimeout,
		wsOpenTimeout:            wsOpenTimeout,
		clientWSPingInterval:     clientWSPingInterval,
		clientWSPingDebug:        clientWSPingDebug,
		wsReadLimitBytes:         shared.WSReadLimitBytes,
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

func (s *Server) startClientWSPingLoop(ctx context.Context, cancel context.CancelFunc, wsClient *websocket.Conn, writeMu *sync.Mutex, uuid string, path string) {
	interval := s.clientWSPingInterval
	if interval <= 0 {
		return
	}
	pongWait := interval*2 + 5*time.Second
	_ = wsClient.SetReadDeadline(time.Now().Add(pongWait))
	wsClient.SetPongHandler(func(appData string) error {
		_ = wsClient.SetReadDeadline(time.Now().Add(pongWait))
		if s.clientWSPingDebug {
			log.Printf("[clockbridge-gateway] ws client pong received uuid=%s path=%s payload=%q", uuid, path, appData)
		}
		return nil
	})
	if s.clientWSPingDebug {
		log.Printf("[clockbridge-gateway] ws client ping loop start uuid=%s path=%s interval=%s", uuid, path, interval)
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if s.clientWSPingDebug {
					log.Printf("[clockbridge-gateway] ws client ping loop stop uuid=%s path=%s reason=context_done", uuid, path)
				}
				return
			case <-ticker.C:
				writeMu.Lock()
				err := wsClient.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(2*time.Second))
				writeMu.Unlock()
				if err != nil {
					log.Printf("[clockbridge-gateway] ws client ping failed uuid=%s path=%s err=%v", uuid, path, err)
					cancel()
					_ = wsClient.Close()
					return
				}
				if s.clientWSPingDebug {
					log.Printf("[clockbridge-gateway] ws client ping sent uuid=%s path=%s", uuid, path)
				}
			}
		}
	}()
}

func (s *Server) setClientWSPongHandler(wsClient *websocket.Conn, uuid string, path string) {
	if !s.clientWSPingDebug {
		return
	}
	wsClient.SetPongHandler(func(appData string) error {
		log.Printf("[clockbridge-gateway] ws client pong received uuid=%s path=%s payload=%q", uuid, path, appData)
		return nil
	})
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func envCommaStringSlice(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func envCommaStringSet(key string) (map[string]struct{}, []string) {
	list := envCommaStringSlice(key)
	set := make(map[string]struct{}, len(list))
	dedup := make([]string, 0, len(list))
	for _, item := range list {
		if _, ok := set[item]; ok {
			continue
		}
		set[item] = struct{}{}
		dedup = append(dedup, item)
	}
	return set, dedup
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/status":
		if !isLocalhostRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		s.mu.lock()
		uuids := make([]string, 0, len(s.agentByUUID))
		proxyModes := make(map[string]string, len(s.agentByUUID))
		proxyModesRequested := make(map[string]string, len(s.agentByUUID))
		proxyModeReasons := make(map[string]string, len(s.agentByUUID))
		targetKinds := make(map[string]string, len(s.agentByUUID))
		activeStreams := make(map[string]int, len(s.agentByUUID))
		agentAgesSeconds := make(map[string]int64, len(s.agentByUUID))
		agentIdleSeconds := make(map[string]int64, len(s.agentByUUID))
		now := time.Now()
		for k, agent := range s.agentByUUID {
			uuids = append(uuids, k)
			proxyModes[k] = agent.proxyModeEffective
			proxyModesRequested[k] = agent.proxyModeRequested
			proxyModeReasons[k] = agent.proxyModeReason
			targetKinds[k] = agent.targetKind
			activeStreams[k] = agent.activeStreamCount()
			agentAgesSeconds[k] = int64(now.Sub(agent.bornAt).Seconds())
			agentIdleSeconds[k] = int64(agent.idleFor().Seconds())
		}
		s.mu.unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                    true,
			"count":                 len(uuids),
			"uuids":                 uuids,
			"proxy_modes":           proxyModes,
			"proxy_modes_requested": proxyModesRequested,
			"proxy_mode_reasons":    proxyModeReasons,
			"target_kinds":          targetKinds,
			"active_streams":        activeStreams,
			"agent_age_seconds":     agentAgesSeconds,
			"agent_idle_seconds":    agentIdleSeconds,
			"proxy_mode_policy": map[string]any{
				"transparent_enabled": s.transparentEnabled,
				"force_legacy_mode":   s.forceLegacyMode,
				"allow_uuids":         append([]string(nil), s.transparentAllowUUIDs...),
				"allow_prefixes":      append([]string(nil), s.transparentAllowPrefixes...),
			},
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

func isLocalhostRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return isLoopbackRemoteAddr(r.RemoteAddr)
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return false
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}

	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	requestedProxyMode := normalizeRegisterProxyMode(r.URL.Query().Get("proxy_mode"), uuid, r.RemoteAddr)
	targetKind := normalizeRegisterTargetKind(r.URL.Query().Get("target_kind"), uuid, r.RemoteAddr)
	effectiveProxyMode, proxyModeReason := s.resolveProxyMode(uuid, requestedProxyMode)

	registerRespHeader := http.Header{}
	registerRespHeader.Set(shared.HeaderProxyMode, effectiveProxyMode)
	registerRespHeader.Set(shared.HeaderProxyModeReason, proxyModeReason)
	ws, err := s.upgrader.Upgrade(w, r, registerRespHeader)
	if err != nil {
		return
	}
	ws.SetReadLimit(s.wsReadLimitBytes)

	s.mu.lock()
	if old := s.agentByUUID[uuid]; old != nil {
		s.mu.unlock()
		log.Printf("[clockbridge-gateway] reject register uuid=%s remote=%s reason=uuid_already_active", uuid, r.RemoteAddr)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "uuid already active"), time.Now().Add(2*time.Second))
		_ = ws.Close()
		return
	}
	c := newAgentConn(uuid, requestedProxyMode, effectiveProxyMode, proxyModeReason, targetKind, ws, s.maxStreamsPerAgent)
	s.agentByUUID[uuid] = c
	s.mu.unlock()
	log.Printf(
		"[clockbridge-gateway] agent online uuid=%s requested_mode=%s mode=%s reason=%s target_kind=%s remote=%s",
		uuid,
		requestedProxyMode,
		effectiveProxyMode,
		proxyModeReason,
		targetKind,
		r.RemoteAddr,
	)

	go c.readLoop()
	go func() {
		<-c.closedCh
		s.removeAgentIfMatch(uuid, c, c.closeReason())
	}()

	if s.heartbeatTimeout > 0 {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-c.closedCh:
					return
				case <-ticker.C:
					idle := c.idleFor()
					if idle > s.heartbeatTimeout {
						reason := fmt.Sprintf("heartbeat timeout idle=%s timeout=%s", idle, s.heartbeatTimeout)
						s.removeAgentIfMatch(uuid, c, reason)
						_ = ws.Close()
						c.closeWithErr(errors.New(reason))
						return
					}
				}
			}
		}()
	}
}

func (s *Server) removeAgentIfMatch(uuid string, c *agentConn, reason string) {
	if reason == "" {
		reason = "unknown"
	}
	removed := false
	s.mu.lock()
	if s.agentByUUID[uuid] == c {
		delete(s.agentByUUID, uuid)
		removed = true
	}
	s.mu.unlock()
	if removed {
		log.Printf(
			"[clockbridge-gateway] agent offline uuid=%s requested_mode=%s mode=%s reason=%s",
			uuid,
			c.proxyModeRequested,
			c.proxyModeEffective,
			reason,
		)
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
		if agent.proxyModeEffective == shared.ProxyModeTransparentTunnel {
			if agent.targetKind == shared.TargetKindWebSocket {
				s.handleClientWebSocketTransparentRaw(w, r, agent, suffixPath)
			} else {
				s.handleClientWebSocketTransparent(w, r, agent, suffixPath)
			}
		} else {
			s.handleClientWebSocket(w, r, agent, suffixPath)
		}
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
	rawQuery := r.URL.RawQuery
	applyQueryBearerAuth(headers, &rawQuery)
	trailerKeys := shared.TrailerKeysFromHeader(headers)
	if len(trailerKeys) == 0 && len(r.Trailer) > 0 {
		trailerKeys = shared.HeaderKeys(r.Trailer)
	}
	shared.StripConnectionHeaders(headers)

	reqStart := proto.ReqStart{
		Method:      r.Method,
		Path:        suffixPath,
		RawQuery:    rawQuery,
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
	if flusher != nil {
		// Ensure streaming clients (e.g. SSE) receive response headers immediately.
		flusher.Flush()
	}
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
	rawQuery := r.URL.RawQuery
	applyQueryBearerAuth(wsHeaders, &rawQuery)

	reqStart := proto.ReqStart{
		Method:      r.Method,
		Path:        suffixPath,
		RawQuery:    rawQuery,
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
		writeAgentWSOpenErr(w, msg.Message, agent.uuid, r.URL.Path)
		return
	}
	if f.Type != proto.TWsOpenOK {
		_ = st.send(proto.TRst, []byte("agent ws bad handshake"))
		http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
		return
	}

	var ok proto.WsOpenOK
	_ = json.Unmarshal(f.Payload, &ok)
	if !isFramedRelayMode(ok.RelayMode) {
		log.Printf(
			"[clockbridge-gateway] ws open relay mismatch uuid=%s path=%s expected=%s got=%s",
			agent.uuid,
			r.URL.Path,
			proto.WsRelayModeFramed,
			ok.RelayMode,
		)
		_ = st.send(proto.TRst, []byte("agent ws relay mismatch"))
		http.Error(w, "agent ws relay mismatch", http.StatusBadGateway)
		return
	}
	respHeader := http.Header{}
	if ok.Subprotocol != "" {
		sp, err := sanitizeWebSocketHeaderValue(ok.Subprotocol)
		if err != nil {
			_ = st.send(proto.TRst, []byte("agent ws bad subprotocol"))
			http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
			return
		}
		if sp != "" {
			respHeader.Set("Sec-WebSocket-Protocol", sp)
		}
	}
	wsClient, err := s.upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		_ = st.send(proto.TWsClose, proto.MustJSON(proto.WsClose{Code: websocket.CloseProtocolError, Reason: "client upgrade failed"}))
		return
	}
	wsClient.SetReadLimit(s.wsReadLimitBytes)
	s.setClientWSPongHandler(wsClient, agent.uuid, r.URL.Path)
	defer func() { _ = wsClient.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var wsWriteMu sync.Mutex
	s.startClientWSPingLoop(ctx, cancel, wsClient, &wsWriteMu, agent.uuid, r.URL.Path)

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
				log.Printf("[clockbridge-gateway] ws client read error uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
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
			wsWriteMu.Lock()
			err := wsClient.WriteMessage(mt, data)
			wsWriteMu.Unlock()
			if err != nil {
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
			log.Printf("[clockbridge-gateway] ws upstream close uuid=%s path=%s code=%d reason=%q", agent.uuid, r.URL.Path, code, reason)
			closeDone.Store(true)
			wsWriteMu.Lock()
			_ = wsClient.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(2*time.Second))
			wsWriteMu.Unlock()
			return
		case proto.TRst:
			return
		}
	}
}

func (s *Server) handleClientWebSocketTransparent(w http.ResponseWriter, r *http.Request, agent *agentConn, suffixPath string) {
	// Transparent mode keeps WS payload bytes opaque and forwards them to agent without legacy WS opcode wrapping.
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
		writeAgentWSOpenErr(w, msg.Message, agent.uuid, r.URL.Path)
		return
	}
	if f.Type != proto.TWsOpenOK {
		_ = st.send(proto.TRst, []byte("agent ws bad handshake"))
		http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
		return
	}

	var ok proto.WsOpenOK
	_ = json.Unmarshal(f.Payload, &ok)
	if !isFramedRelayMode(ok.RelayMode) {
		log.Printf(
			"[clockbridge-gateway] ws transparent relay mismatch uuid=%s path=%s expected=%s got=%s",
			agent.uuid,
			r.URL.Path,
			proto.WsRelayModeFramed,
			ok.RelayMode,
		)
		_ = st.send(proto.TRst, []byte("agent ws relay mismatch"))
		http.Error(w, "agent ws relay mismatch", http.StatusBadGateway)
		return
	}
	respHeader := http.Header{}
	if ok.Subprotocol != "" {
		sp, err := sanitizeWebSocketHeaderValue(ok.Subprotocol)
		if err != nil {
			_ = st.send(proto.TRst, []byte("agent ws bad subprotocol"))
			http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
			return
		}
		if sp != "" {
			respHeader.Set("Sec-WebSocket-Protocol", sp)
		}
	}
	wsClient, err := s.upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		_ = st.send(proto.TWsClose, proto.MustJSON(proto.WsClose{Code: websocket.CloseProtocolError, Reason: "client upgrade failed"}))
		return
	}
	wsClient.SetReadLimit(s.wsReadLimitBytes)
	s.setClientWSPongHandler(wsClient, agent.uuid, r.URL.Path)
	defer func() { _ = wsClient.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var wsWriteMu sync.Mutex
	s.startClientWSPingLoop(ctx, cancel, wsClient, &wsWriteMu, agent.uuid, r.URL.Path)

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
				log.Printf("[clockbridge-gateway] ws transparent client read error uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
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
				log.Printf("[clockbridge-gateway] ws transparent send to agent failed uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
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
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				mt = websocket.BinaryMessage
			}
			data := f2.Payload[1:]
			wsWriteMu.Lock()
			err := wsClient.WriteMessage(mt, data)
			wsWriteMu.Unlock()
			if err != nil {
				log.Printf("[clockbridge-gateway] ws transparent write to client failed uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
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
			log.Printf("[clockbridge-gateway] ws transparent upstream close uuid=%s path=%s code=%d reason=%q", agent.uuid, r.URL.Path, code, reason)
			closeDone.Store(true)
			wsWriteMu.Lock()
			_ = wsClient.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(2*time.Second))
			wsWriteMu.Unlock()
			return
		case proto.TRst:
			return
		}
	}
}

func (s *Server) handleClientWebSocketTransparentRaw(w http.ResponseWriter, r *http.Request, agent *agentConn, suffixPath string) {
	// Raw transparent path for ws target:
	// after handshake, relay upgraded bytes opaquely without WebSocket frame parsing.
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
		writeAgentWSOpenErr(w, msg.Message, agent.uuid, r.URL.Path)
		return
	}
	if f.Type != proto.TWsOpenOK {
		_ = st.send(proto.TRst, []byte("agent ws bad handshake"))
		http.Error(w, "agent ws bad handshake", http.StatusBadGateway)
		return
	}

	var ok proto.WsOpenOK
	_ = json.Unmarshal(f.Payload, &ok)
	if !isRawRelayMode(ok.RelayMode) {
		log.Printf(
			"[clockbridge-gateway] ws transparent raw relay mismatch uuid=%s path=%s expected=%s got=%s",
			agent.uuid,
			r.URL.Path,
			proto.WsRelayModeRaw,
			ok.RelayMode,
		)
		_ = st.send(proto.TRst, []byte("agent ws relay mismatch"))
		http.Error(w, "agent ws relay mismatch", http.StatusBadGateway)
		return
	}

	rawConn, hijackedRW, err := hijackAndWriteWebSocketSwitchingProtocols(w, r, ok)
	if err != nil {
		_ = st.send(proto.TRst, []byte("client upgrade failed"))
		log.Printf("[clockbridge-gateway] ws transparent raw hijack failed uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
		return
	}
	defer func() { _ = rawConn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var closeOnce sync.Once
	closeStream := func(reason string) {
		closeOnce.Do(func() {
			_ = st.send(proto.TRst, []byte(reason))
			st.closeWithErr(errors.New(reason))
			cancel()
		})
	}

	go func() {
		defer cancel()

		if buffered := hijackedRW.Reader.Buffered(); buffered > 0 {
			buf := make([]byte, buffered)
			if _, err := io.ReadFull(hijackedRW.Reader, buf); err != nil {
				closeStream("raw client buffered read failed")
				return
			}
			if err := st.send(proto.TWsC2A, buf); err != nil {
				closeStream("raw send to agent failed")
				return
			}
		}

		buf := make([]byte, 32*1024)
		for {
			n, err := rawConn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if err2 := st.send(proto.TWsC2A, payload); err2 != nil {
					closeStream("raw send to agent failed")
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					closeStream("raw client closed")
				} else {
					log.Printf("[clockbridge-gateway] ws transparent raw client read error uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
					closeStream("raw client read failed")
				}
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
			closeStream("agent stream closed")
			return
		}
		switch f2.Type {
		case proto.TWsA2C:
			if len(f2.Payload) == 0 {
				continue
			}
			if _, err := rawConn.Write(f2.Payload); err != nil {
				log.Printf("[clockbridge-gateway] ws transparent raw write to client failed uuid=%s path=%s err=%v", agent.uuid, r.URL.Path, err)
				closeStream("raw client write failed")
				return
			}
		case proto.TWsClose, proto.TRst:
			closeStream("agent closed stream")
			return
		}
	}
}

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(key) + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func isFramedRelayMode(mode string) bool {
	value := strings.ToLower(strings.TrimSpace(mode))
	return value == "" || value == proto.WsRelayModeFramed
}

func isRawRelayMode(mode string) bool {
	value := strings.ToLower(strings.TrimSpace(mode))
	return value == proto.WsRelayModeRaw
}

func sanitizeWebSocketHeaderValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	for _, r := range value {
		if r == '\r' || r == '\n' || r == 0 {
			return "", fmt.Errorf("contains forbidden newline/null")
		}
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return "", fmt.Errorf("contains control character")
		}
	}
	return value, nil
}

func writeAgentWSOpenErr(w http.ResponseWriter, detail string, uuid string, path string) {
	if strings.TrimSpace(detail) != "" {
		log.Printf("[clockbridge-gateway] ws open failed uuid=%s path=%s detail=%q", uuid, path, detail)
	}
	http.Error(w, "agent ws open failed", http.StatusBadGateway)
}

func hijackAndWriteWebSocketSwitchingProtocols(w http.ResponseWriter, r *http.Request, openOK proto.WsOpenOK) (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	subprotocol, err := sanitizeWebSocketHeaderValue(openOK.Subprotocol)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid websocket subprotocol: %w", err)
	}
	extensions := make([]string, 0, len(openOK.Extensions))
	for _, extRaw := range openOK.Extensions {
		ext, err := sanitizeWebSocketHeaderValue(extRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid websocket extensions: %w", err)
		}
		if ext == "" {
			continue
		}
		extensions = append(extensions, ext)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, nil, errors.New("missing Sec-WebSocket-Key")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if _, err := rw.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if _, err := rw.WriteString("Sec-WebSocket-Accept: " + websocketAccept(key) + "\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if subprotocol != "" {
		if _, err := rw.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n"); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	for _, ext := range extensions {
		if _, err := rw.WriteString("Sec-WebSocket-Extensions: " + ext + "\r\n"); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	if _, err := rw.WriteString("\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
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

func applyQueryBearerAuth(h http.Header, rawQuery *string) {
	if rawQuery == nil || strings.TrimSpace(*rawQuery) == "" {
		return
	}
	q, err := url.ParseQuery(*rawQuery)
	if err != nil {
		return
	}

	auth := strings.TrimSpace(firstQueryValue(q, "authorization"))
	if auth == "" {
		if token := strings.TrimSpace(firstQueryValue(q, "access_token")); token != "" {
			auth = "Bearer " + token
		}
	}
	if auth == "" {
		if token := strings.TrimSpace(firstQueryValue(q, "token")); token != "" {
			auth = "Bearer " + token
		}
	}
	if auth != "" {
		if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			auth = "Bearer " + auth
		}
		h.Set("Authorization", auth)
	}

	q.Del("authorization")
	q.Del("access_token")
	q.Del("token")
	*rawQuery = q.Encode()
}

func firstQueryValue(q url.Values, key string) string {
	for _, v := range q[key] {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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

func (w *logResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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

func normalizeRegisterProxyMode(raw string, uuid string, remoteAddr string) string {
	mode, err := shared.NormalizeProxyMode(raw)
	if err != nil {
		log.Printf(
			"[clockbridge-gateway] register invalid proxy_mode uuid=%s remote=%s raw=%q fallback=%s err=%v",
			uuid,
			remoteAddr,
			raw,
			shared.ProxyModeLegacyFramed,
			err,
		)
		return shared.ProxyModeLegacyFramed
	}
	return mode
}

func normalizeRegisterTargetKind(raw string, uuid string, remoteAddr string) string {
	kind, err := shared.NormalizeTargetKind(raw)
	if err != nil {
		log.Printf(
			"[clockbridge-gateway] register invalid target_kind uuid=%s remote=%s raw=%q fallback=%s err=%v",
			uuid,
			remoteAddr,
			raw,
			shared.TargetKindUnknown,
			err,
		)
		return shared.TargetKindUnknown
	}
	return kind
}

func (s *Server) resolveProxyMode(uuid string, requested string) (string, string) {
	if requested != shared.ProxyModeTransparentTunnel {
		return shared.ProxyModeLegacyFramed, "legacy_requested"
	}
	if s.forceLegacyMode {
		return shared.ProxyModeLegacyFramed, "force_legacy_mode"
	}
	if !s.transparentEnabled {
		return shared.ProxyModeLegacyFramed, "transparent_disabled"
	}
	if !s.isTransparentAllowed(uuid) {
		return shared.ProxyModeLegacyFramed, "transparent_not_whitelisted"
	}
	return shared.ProxyModeTransparentTunnel, "transparent_allowed"
}

func (s *Server) isTransparentAllowed(uuid string) bool {
	if uuid == "" {
		return false
	}
	if len(s.transparentAllowUUIDSet) > 0 {
		if _, ok := s.transparentAllowUUIDSet[uuid]; ok {
			return true
		}
	}
	for _, prefix := range s.transparentAllowPrefixes {
		if strings.HasPrefix(uuid, prefix) {
			return true
		}
	}
	return false
}
