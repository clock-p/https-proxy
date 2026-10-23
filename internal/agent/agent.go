package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
	"github.com/clock-p/clockbridge/internal/shared"
	"github.com/gorilla/websocket"
)

type Agent struct {
	// OnStatus 在连接循环内同步调用；接收者不能阻塞或修改回调中的快照。
	OnStatus            func(ConnectionStatus)
	registerURL         *url.URL
	registerDialAddr    string
	registerXToken      string
	registerBearerToken string
	registerProxyMode   string
	registerTargetKind  string
	targetBase          *url.URL
	wsReadLimit         int64

	httpClient    *http.Client
	httpTransport *http.Transport
}

type ConnectionStatus struct {
	State           string     `json:"state"`
	Attempt         int        `json:"attempt"`
	LastError       string     `json:"last_error,omitempty"`
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
}

type connState struct {
	a *Agent

	ws          *websocket.Conn
	writeMu     sync.Mutex
	lastPongLog time.Time
	onPong      func()
	proxyMode   string

	streamMu sync.Mutex
	streams  map[uint32]*stream
	wg       sync.WaitGroup
}

type stream struct {
	id        uint32
	rx        chan proto.Frame
	closed    chan struct{}
	closeOnce sync.Once
}

func New(registerURL *url.URL, registerDialAddr string, registerXToken string, registerBearerToken string, targetBase *url.URL) *Agent {
	firstRespTimeout := envDuration("HTTPS_PROXY_FIRST_RESPONSE_TIMEOUT", 30*time.Second)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: firstRespTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	log.Printf("[clockbridge-cli] upstream first response timeout=%s", firstRespTimeout)
	return &Agent{
		registerURL:         registerURL,
		registerDialAddr:    registerDialAddr,
		registerXToken:      registerXToken,
		registerBearerToken: registerBearerToken,
		registerProxyMode:   proxyModeFromRegisterURL(registerURL),
		registerTargetKind:  targetKindFromRegisterURL(registerURL),
		targetBase:          targetBase,
		wsReadLimit:         shared.WSReadLimitBytes,
		httpTransport:       transport,
		httpClient: &http.Client{
			Transport: transport,
		},
	}
}

func targetKindFromRegisterURL(registerURL *url.URL) string {
	if registerURL == nil {
		return shared.TargetKindUnknown
	}
	kind, err := shared.NormalizeTargetKind(registerURL.Query().Get("target_kind"))
	if err != nil {
		return shared.TargetKindUnknown
	}
	return kind
}

func proxyModeFromRegisterURL(registerURL *url.URL) string {
	if registerURL == nil {
		return shared.ProxyModeLegacyFramed
	}
	mode, err := shared.NormalizeProxyMode(registerURL.Query().Get("proxy_mode"))
	if err != nil {
		return shared.ProxyModeLegacyFramed
	}
	return mode
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

func (a *Agent) Run(ctx context.Context) error {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	attempt := 0
	status := ConnectionStatus{}
	report := func(state string, err error, wait time.Duration) {
		status.State, status.Attempt = state, attempt
		status.LastError = ""
		if err != nil {
			status.LastError = err.Error()
		}
		status.NextRetryAt = nil
		if wait > 0 {
			next := time.Now().UTC().Add(wait)
			status.NextRetryAt = &next
		}
		if a.OnStatus != nil {
			a.OnStatus(status)
		}
	}
	defer func() { report("stopped", nil, 0) }()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		report("connecting", nil, 0)

		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		if a.registerDialAddr != "" {
			dialAddr := a.registerDialAddr
			dialer.NetDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, dialAddr)
			}
		}
		h := http.Header{}
		if a.registerXToken != "" {
			h.Set("X-Token", a.registerXToken)
		}
		if a.registerBearerToken != "" {
			h.Set("Authorization", "Bearer "+a.registerBearerToken)
		}

		ws, resp, err := dialer.DialContext(ctx, a.registerURL.String(), h)
		if err != nil {
			attempt++
			wait := reconnectBackoff(rnd, attempt)
			report("backoff", err, wait)
			if hs := handshakeSummary(resp); hs != "" {
				log.Printf(
					"[clockbridge-cli] dial failed (attempt=%d) register_url=%s err=%v handshake={%s}; retry in %s",
					attempt,
					a.registerURL.String(),
					err,
					hs,
					wait,
				)
			} else {
				log.Printf("[clockbridge-cli] dial failed (attempt=%d) register_url=%s err=%v; retry in %s", attempt, a.registerURL.String(), err, wait)
			}
			if !sleepWithContext(ctx, wait) {
				return ctx.Err()
			}
			continue
		}

		// 握手成功不代表连接稳定，短暂连接不能清掉累计退避。
		connectedAt := time.Now().UTC()
		status.LastConnectedAt = &connectedAt
		report("connected", nil, 0)
		negotiatedProxyMode := negotiatedProxyModeFromHandshake(resp, shared.ProxyModeLegacyFramed)
		if negotiatedProxyMode != a.registerProxyMode {
			log.Printf(
				"[clockbridge-cli] proxy mode negotiated requested=%s effective=%s",
				a.registerProxyMode,
				negotiatedProxyMode,
			)
		}

		s := &connState{
			a:         a,
			ws:        ws,
			proxyMode: negotiatedProxyMode,
			streams:   make(map[uint32]*stream),
			onPong: func() {
				now := time.Now().UTC()
				status.LastHeartbeatAt = &now
				report("connected", nil, 0)
			},
		}

		log.Printf(
			"[clockbridge-cli] connected register_url=%s target=%s requested_mode=%s effective_mode=%s local=%s remote=%s handshake={%s}",
			a.registerURL.String(),
			a.targetBase.String(),
			a.registerProxyMode,
			negotiatedProxyMode,
			connAddrLocal(ws),
			connAddrRemote(ws),
			handshakeSummary(resp),
		)

		// Ensure ctrl+C (ctx cancellation) unblocks ReadMessage promptly.
		connCtx, cancel := context.WithCancel(ctx)
		go func() {
			<-connCtx.Done()
			s.writeMu.Lock()
			_ = s.ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = s.ws.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "agent shutting down"),
				time.Now().Add(2*time.Second),
			)
			s.writeMu.Unlock()
			_ = s.ws.SetReadDeadline(time.Now())
			_ = s.ws.Close()
		}()

		err = s.run(connCtx)
		cancel()

		s.closeAllStreams()
		s.wg.Wait()
		_ = ws.Close()
		if a.httpTransport != nil {
			a.httpTransport.CloseIdleConnections()
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(connectedAt) >= 30*time.Second && status.LastHeartbeatAt != nil && status.LastHeartbeatAt.After(connectedAt) {
			attempt = 0
		}

		attempt++
		wait := reconnectBackoff(rnd, attempt)
		state := "backoff"
		if code, _, ok := shared.CloseFromErr(err); ok && code == 4009 {
			// 旧连接可能尚未被网关清理；保留身份并等待，不抢占、不永久退出。
			state = "identity_conflict"
			if wait < 5*time.Second {
				wait = 5 * time.Second
			}
		}
		report(state, err, wait)
		log.Printf("[clockbridge-cli] disconnected (attempt=%d) err=%v; reconnect in %s", attempt, err, wait)
		if !sleepWithContext(ctx, wait) {
			return ctx.Err()
		}
	}
}

func reconnectBackoff(rnd *rand.Rand, attempt int) time.Duration {
	// Exponential backoff with jitter, capped.
	if attempt < 1 {
		attempt = 1
	}
	base := time.Second
	max := 30 * time.Second
	d := base << minInt(attempt-1, 5) // 1s,2s,4s,8s,16s,32s
	if d > max {
		d = max
	}
	// 0.8x - 1.2x jitter.
	j := 0.8 + rnd.Float64()*0.4
	return time.Duration(float64(d) * j)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func connAddrLocal(ws *websocket.Conn) string {
	if ws == nil || ws.UnderlyingConn() == nil || ws.UnderlyingConn().LocalAddr() == nil {
		return "-"
	}
	return ws.UnderlyingConn().LocalAddr().String()
}

func connAddrRemote(ws *websocket.Conn) string {
	if ws == nil || ws.UnderlyingConn() == nil || ws.UnderlyingConn().RemoteAddr() == nil {
		return "-"
	}
	return ws.UnderlyingConn().RemoteAddr().String()
}

func handshakeSummary(resp *http.Response) string {
	if resp == nil {
		return ""
	}

	status := resp.Status
	location := resp.Header.Get("Location")
	bodySnippet := ""
	if resp.Body != nil {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if err == nil {
			bodySnippet = string(data)
		}
	}
	if bodySnippet == "" {
		bodySnippet = "-"
	}
	if location == "" {
		return fmt.Sprintf("status=%s body=%q", status, bodySnippet)
	}
	return fmt.Sprintf("status=%s location=%q body=%q", status, location, bodySnippet)
}

func negotiatedProxyModeFromHandshake(resp *http.Response, fallback string) string {
	if strings.TrimSpace(fallback) == "" {
		fallback = shared.ProxyModeLegacyFramed
	}
	if resp == nil {
		return fallback
	}
	raw := strings.TrimSpace(resp.Header.Get(shared.HeaderProxyMode))
	if raw == "" {
		return fallback
	}
	mode, err := shared.NormalizeProxyMode(raw)
	if err != nil {
		log.Printf(
			"[clockbridge-cli] ignore invalid handshake proxy mode header=%q fallback=%s err=%v",
			raw,
			fallback,
			err,
		)
		return fallback
	}
	return mode
}

func (s *connState) run(ctx context.Context) error {
	s.ws.SetPongHandler(func(appData string) error {
		_ = s.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		if s.onPong != nil {
			s.onPong()
		}
		now := time.Now()
		if s.lastPongLog.IsZero() || now.Sub(s.lastPongLog) >= 60*time.Second {
			log.Printf("[clockbridge-cli] heartbeat pong register_url=%s", s.a.registerURL.String())
			s.lastPongLog = now
		}
		return nil
	})
	s.ws.SetCloseHandler(func(code int, text string) error {
		log.Printf("[clockbridge-cli] register close frame code=%d reason=%q", code, text)
		msg := websocket.FormatCloseMessage(code, "")
		_ = s.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
		return nil
	})
	s.ws.SetReadLimit(s.a.wsReadLimit + proto.WebSocketFrameOverhead)
	_ = s.ws.SetReadDeadline(time.Now().Add(90 * time.Second))

	go s.pingLoop(ctx)
	return s.readLoop(ctx)
}

func (s *connState) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := s.ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(2*time.Second))
			s.writeMu.Unlock()
			if err != nil {
				log.Printf("[clockbridge-cli] heartbeat ping failed register_url=%s err=%v", s.a.registerURL.String(), err)
				_ = s.ws.Close()
				return
			}
		}
	}
}

func (s *connState) readLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		mt, msg, err := s.ws.ReadMessage()
		if err != nil {
			if code, text, ok := shared.CloseFromErr(err); ok {
				log.Printf("[clockbridge-cli] read loop closed code=%d reason=%q", code, text)
			} else if errors.Is(err, net.ErrClosed) {
				log.Printf("[clockbridge-cli] read loop connection closed err=%v", err)
			} else {
				log.Printf("[clockbridge-cli] read loop error err=%v", err)
			}
			return err
		}
		if mt != websocket.BinaryMessage {
			if mt != websocket.PingMessage && mt != websocket.PongMessage {
				log.Printf("[clockbridge-cli] ignore ws message type=%d len=%d", mt, len(msg))
			}
			continue
		}
		f, err := proto.DecodeFrame(msg)
		if err != nil {
			log.Printf("[clockbridge-cli] drop bad frame len=%d err=%v", len(msg), err)
			continue
		}
		st := s.getOrCreateStream(ctx, f.Stream)
		_ = s.deliverFrame(st, f)
	}
}

func (s *connState) getOrCreateStream(ctx context.Context, id uint32) *stream {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if st := s.streams[id]; st != nil {
		return st
	}
	st := &stream{id: id, rx: make(chan proto.Frame, 128), closed: make(chan struct{})}
	s.streams[id] = st
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.handleStream(ctx, st)
	}()
	return st
}

func (s *connState) forgetStream(id uint32) {
	s.streamMu.Lock()
	st := s.streams[id]
	delete(s.streams, id)
	s.streamMu.Unlock()
	if st != nil {
		st.close()
	}
}

func (s *stream) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func (s *connState) send(t byte, stream uint32, payload []byte) error {
	b := proto.EncodeFrame(t, stream, payload)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := s.ws.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		log.Printf("[clockbridge-cli] send frame failed stream=%d type=%d err=%v", stream, t, err)
	}
	return err
}

func (s *connState) deliverFrame(st *stream, f proto.Frame) bool {
	select {
	case <-st.closed:
		return false
	case st.rx <- f:
		return true
	default:
		log.Printf("[clockbridge-cli] stream rx overflow stream=%d", st.id)
		_ = s.send(proto.TRst, st.id, []byte("stream rx overflow"))
		s.forgetStream(st.id)
		return false
	}
}

func (s *connState) handleStream(ctx context.Context, st *stream) {
	defer s.forgetStream(st.id)

	var f proto.Frame
	select {
	case <-st.closed:
		return
	case f = <-st.rx:
	}
	if f.Type != proto.TReqStart {
		log.Printf("[clockbridge-cli] invalid first frame stream=%d type=%d", st.id, f.Type)
		_ = s.send(proto.TRst, st.id, []byte("missing req_start"))
		return
	}
	var rs proto.ReqStart
	if err := json.Unmarshal(f.Payload, &rs); err != nil {
		log.Printf("[clockbridge-cli] bad req_start stream=%d err=%v", st.id, err)
		_ = s.send(proto.TRst, st.id, []byte("bad req_start"))
		return
	}

	if rs.IsWebSocket {
		if s.proxyMode == shared.ProxyModeTransparentTunnel {
			s.handleWebSocketTransparent(ctx, st, rs)
			return
		}
		s.handleWebSocket(ctx, st, rs)
		return
	}
	if isTCPTarget(s.a.targetBase) {
		log.Printf("[clockbridge-cli] tcp target does not support plain HTTP stream=%d path=%s", st.id, rs.Path)
		_ = s.send(proto.TRst, st.id, []byte("tcp target only supports websocket upgrade"))
		return
	}
	s.handleHTTP(ctx, st, rs)
}

func (s *connState) handleHTTP(ctx context.Context, st *stream, rs proto.ReqStart) {
	pr, pw := io.Pipe()
	bodyClosed := make(chan struct{})
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, rs.Method, "", pr)
	if err != nil {
		log.Printf("[clockbridge-cli] build upstream request failed stream=%d method=%s path=%s err=%v", st.id, rs.Method, rs.Path, err)
		_ = s.send(proto.TRst, st.id, []byte("bad request"))
		return
	}

	go func() {
		defer close(bodyClosed)
		defer func() { _ = pw.Close() }()
		for {
			select {
			case <-st.closed:
				cancel()
				return
			case <-reqCtx.Done():
				return
			case f := <-st.rx:
				switch f.Type {
				case proto.TReqData:
					if len(f.Payload) > 0 {
						if _, err := pw.Write(f.Payload); err != nil {
							log.Printf("[clockbridge-cli] upstream request body write failed stream=%d err=%v", st.id, err)
							cancel()
							return
						}
					}
				case proto.TReqTrailer:
					var tr map[string][]string
					_ = json.Unmarshal(f.Payload, &tr)
					if req.Trailer == nil {
						req.Trailer = make(http.Header)
					}
					for k, vv := range tr {
						req.Trailer[http.CanonicalHeaderKey(k)] = vv
					}
				case proto.TReqEnd:
					return
				case proto.TRst:
					cancel()
					return
				}
			}
		}
	}()

	target := shared.JoinURL(s.a.targetBase, rs.Path)
	target.RawQuery = rs.RawQuery

	req.URL = target
	req.Host = ""
	shared.CopyHeaders(req.Header, http.Header(rs.Header))
	shared.StripConnectionHeaders(req.Header)
	if rs.Host != "" {
		req.Host = rs.Host
	}
	if len(rs.TrailerKeys) > 0 {
		req.Trailer = make(http.Header)
		for _, k := range rs.TrailerKeys {
			req.Trailer[http.CanonicalHeaderKey(k)] = nil
		}
		req.Header.Del("Content-Length")
		req.ContentLength = -1
	} else if cl := req.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			req.ContentLength = n
		}
	}

	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			h := http.Header(header)
			shared.StripConnectionHeaders(h)
			_ = s.send(proto.TResStart, st.id, proto.MustJSON(proto.ResStart{Status: code, Header: h, Interim: true}))
			return nil
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := s.a.httpClient.Do(req)
	if err != nil {
		log.Printf(
			"[clockbridge-cli] upstream request failed stream=%d method=%s target=%s err=%v",
			st.id,
			rs.Method,
			target.String(),
			err,
		)
		_ = s.send(proto.TResStart, st.id, proto.MustJSON(proto.ResStart{Status: http.StatusBadGateway}))
		_ = s.send(proto.TResEnd, st.id, nil)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	resHeader := resp.Header.Clone()
	shared.StripConnectionHeaders(resHeader)
	resStart := proto.ResStart{
		Status:      resp.StatusCode,
		Header:      resHeader,
		TrailerKeys: shared.HeaderKeys(resp.Trailer),
	}
	if err := s.send(proto.TResStart, st.id, proto.MustJSON(resStart)); err != nil {
		cancel()
		return
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if err2 := s.send(proto.TResData, st.id, buf[:n]); err2 != nil {
				cancel()
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[clockbridge-cli] upstream response read failed stream=%d target=%s err=%v", st.id, target.String(), err)
			}
			break
		}
	}

	if len(resp.Trailer) > 0 {
		if err := s.send(proto.TResTrailer, st.id, proto.MustJSON(map[string][]string(resp.Trailer))); err != nil {
			cancel()
			return
		}
	}
	_ = s.send(proto.TResEnd, st.id, nil)

	<-bodyClosed
}

func (s *connState) handleWebSocketTransparent(ctx context.Context, st *stream, rs proto.ReqStart) {
	if isTCPTarget(s.a.targetBase) {
		s.handleWebSocketToTCPTransparent(ctx, st, rs)
		return
	}
	if isWebSocketTarget(s.a.targetBase) {
		if !shouldUseRawTransparentWS(s.a.registerTargetKind) {
			// Backward compatibility/safety: if register target_kind does not explicitly indicate ws,
			// keep using framed relay to stay aligned with gateway selection.
			log.Printf(
				"[clockbridge-cli] transparent ws raw relay disabled due to target_kind=%s stream=%d target=%s",
				s.a.registerTargetKind,
				st.id,
				s.a.targetBase.String(),
			)
			s.handleWebSocket(ctx, st, rs)
			return
		}
		s.handleWebSocketToWSTransparent(ctx, st, rs)
		return
	}
	log.Printf(
		"[clockbridge-cli] transparent mode unsupported target stream=%d target=%s",
		st.id,
		s.a.targetBase.String(),
	)
	_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: "transparent mode supports only tcp/ws/wss target"}))
}

func (s *connState) handleWebSocket(ctx context.Context, st *stream, rs proto.ReqStart) {
	if isTCPTarget(s.a.targetBase) {
		s.handleWebSocketToTCP(ctx, st, rs)
		return
	}

	targetHTTP := shared.JoinURL(s.a.targetBase, rs.Path)
	targetHTTP.RawQuery = rs.RawQuery
	targetWS := shared.ToWebSocketURL(targetHTTP)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	h := http.Header{}
	shared.CopyHeadersForWebSocket(h, http.Header(rs.Header))
	if rs.Host != "" {
		h.Set("Host", rs.Host)
	}

	wsUp, _, err := dialer.DialContext(ctx, targetWS.String(), h)
	if err != nil {
		log.Printf("[clockbridge-cli] upstream websocket dial failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	wsUp.SetReadLimit(s.a.wsReadLimit)
	defer func() { _ = wsUp.Close() }()

	sub := wsUp.Subprotocol()
	if err := s.send(proto.TWsOpenOK, st.id, proto.MustJSON(proto.WsOpenOK{
		Subprotocol: sub,
		RelayMode:   proto.WsRelayModeFramed,
	})); err != nil {
		return
	}

	var closeOnce sync.Once
	sendCloseToGateway := func(code int, reason string) {
		closeOnce.Do(func() {
			_ = s.send(proto.TWsClose, st.id, proto.MustJSON(proto.WsClose{Code: code, Reason: reason}))
			st.close()
		})
	}

	go func() {
		for {
			mt, data, err := wsUp.ReadMessage()
			if err != nil {
				log.Printf("[clockbridge-cli] upstream websocket read failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
				if code, reason, ok := shared.CloseFromErr(err); ok {
					sendCloseToGateway(code, reason)
				} else {
					sendCloseToGateway(websocket.CloseGoingAway, "upstream closed")
				}
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			payload := append([]byte{byte(mt)}, data...)
			if err := s.send(proto.TWsA2C, st.id, payload); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-st.closed:
			return
		case <-ctx.Done():
			return
		case f := <-st.rx:
			switch f.Type {
			case proto.TWsC2A:
				if len(f.Payload) == 0 {
					continue
				}
				mt := int(f.Payload[0])
				data := f.Payload[1:]
				if err := wsUp.WriteMessage(mt, data); err != nil {
					log.Printf("[clockbridge-cli] upstream websocket write failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
					if code, reason, ok := shared.CloseFromErr(err); ok {
						sendCloseToGateway(code, reason)
					} else {
						sendCloseToGateway(websocket.CloseGoingAway, "upstream write failed")
					}
					return
				}
			case proto.TWsClose:
				var c proto.WsClose
				_ = json.Unmarshal(f.Payload, &c)
				code := shared.SanitizeCloseCode(c.Code)
				reason := c.Reason
				log.Printf("[clockbridge-cli] gateway websocket close stream=%d code=%d reason=%q", st.id, code, reason)
				_ = wsUp.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(2*time.Second))
				return
			case proto.TRst:
				log.Printf("[clockbridge-cli] gateway reset websocket stream=%d", st.id)
				return
			}
		}
	}
}

func isTCPTarget(target *url.URL) bool {
	if target == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(target.Scheme), "tcp")
}

func isWebSocketTarget(target *url.URL) bool {
	if target == nil {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(target.Scheme))
	return scheme == "ws" || scheme == "wss"
}

func shouldUseRawTransparentWS(registerTargetKind string) bool {
	return strings.EqualFold(strings.TrimSpace(registerTargetKind), shared.TargetKindWebSocket)
}

func dialWebSocketUpstreamRaw(ctx context.Context, targetWS *url.URL, rs proto.ReqStart) (net.Conn, *bufio.Reader, string, []string, error) {
	if targetWS == nil {
		return nil, nil, "", nil, errors.New("invalid websocket target: nil")
	}
	addr := strings.TrimSpace(targetWS.Host)
	if addr == "" {
		return nil, nil, "", nil, errors.New("invalid websocket target: empty host")
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var (
		conn net.Conn
		err  error
	)
	switch strings.ToLower(strings.TrimSpace(targetWS.Scheme)) {
	case "ws":
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	case "wss":
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config: &tls.Config{
				ServerName: targetWS.Hostname(),
			},
		}
		conn, err = tlsDialer.DialContext(ctx, "tcp", addr)
	default:
		return nil, nil, "", nil, fmt.Errorf("unsupported websocket scheme %q", targetWS.Scheme)
	}
	if err != nil {
		return nil, nil, "", nil, err
	}

	hostHeader := strings.TrimSpace(rs.Host)
	if hostHeader == "" {
		hostHeader = targetWS.Host
	}
	method := strings.TrimSpace(rs.Method)
	if method == "" {
		method = http.MethodGet
	}
	requestURI := targetWS.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}

	bw := bufio.NewWriter(conn)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", method, requestURI); err != nil {
		_ = conn.Close()
		return nil, nil, "", nil, err
	}
	if _, err := fmt.Fprintf(bw, "Host: %s\r\n", hostHeader); err != nil {
		_ = conn.Close()
		return nil, nil, "", nil, err
	}
	for k, vv := range http.Header(rs.Header) {
		if strings.EqualFold(k, "Host") {
			continue
		}
		ck := textproto.CanonicalMIMEHeaderKey(k)
		for _, v := range vv {
			if _, err := fmt.Fprintf(bw, "%s: %s\r\n", ck, v); err != nil {
				_ = conn.Close()
				return nil, nil, "", nil, err
			}
		}
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		_ = conn.Close()
		return nil, nil, "", nil, err
	}
	if err := bw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, "", nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		_ = conn.Close()
		return nil, nil, "", nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		_ = conn.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, nil, "", nil, fmt.Errorf("upstream handshake failed: %s", msg)
	}
	_ = resp.Body.Close()
	subprotocol := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Protocol"))
	extensions := append([]string(nil), resp.Header.Values("Sec-WebSocket-Extensions")...)
	return conn, br, subprotocol, extensions, nil
}

func (s *connState) handleWebSocketToWSTransparent(ctx context.Context, st *stream, rs proto.ReqStart) {
	targetWS := shared.JoinURL(s.a.targetBase, rs.Path)
	targetWS.RawQuery = rs.RawQuery

	upConn, upReader, subprotocol, extensions, err := dialWebSocketUpstreamRaw(ctx, targetWS, rs)
	if err != nil {
		log.Printf("[clockbridge-cli] upstream transparent websocket dial failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	defer func() { _ = upConn.Close() }()

	if err := s.send(proto.TWsOpenOK, st.id, proto.MustJSON(proto.WsOpenOK{
		Subprotocol: subprotocol,
		RelayMode:   proto.WsRelayModeRaw,
		Extensions:  extensions,
	})); err != nil {
		return
	}

	var closeOnce sync.Once
	closeStream := func(reason string) {
		closeOnce.Do(func() {
			_ = s.send(proto.TRst, st.id, []byte(reason))
			st.close()
		})
	}

	go func() {
		if buffered := upReader.Buffered(); buffered > 0 {
			buf := make([]byte, buffered)
			if _, err := io.ReadFull(upReader, buf); err != nil {
				closeStream("raw upstream buffered read failed")
				return
			}
			if err := s.send(proto.TWsA2C, st.id, buf); err != nil {
				return
			}
		}

		buf := make([]byte, 32*1024)
		for {
			n, err := upConn.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if err2 := s.send(proto.TWsA2C, st.id, payload); err2 != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					closeStream("raw upstream closed")
				} else {
					log.Printf("[clockbridge-cli] upstream transparent websocket read failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
					closeStream("raw upstream read failed")
				}
				return
			}
		}
	}()

	for {
		select {
		case <-st.closed:
			return
		case <-ctx.Done():
			return
		case f := <-st.rx:
			switch f.Type {
			case proto.TWsC2A:
				if len(f.Payload) == 0 {
					continue
				}
				if _, err := upConn.Write(f.Payload); err != nil {
					log.Printf("[clockbridge-cli] upstream transparent websocket write failed stream=%d target=%s err=%v", st.id, targetWS.String(), err)
					closeStream("raw upstream write failed")
					return
				}
			case proto.TWsClose, proto.TRst:
				log.Printf("[clockbridge-cli] gateway closed websocket stream=%d (transparent ws raw)", st.id)
				closeStream("gateway closed stream")
				return
			}
		}
	}
}

func (s *connState) handleWebSocketToTCPTransparent(ctx context.Context, st *stream, _ proto.ReqStart) {
	dialAddr := strings.TrimSpace(s.a.targetBase.Host)
	if dialAddr == "" {
		log.Printf("[clockbridge-cli] upstream tcp target host is empty stream=%d", st.id)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: "invalid tcp target: empty host"}))
		return
	}
	tcpConn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		log.Printf("[clockbridge-cli] upstream tcp dial failed stream=%d target=%s err=%v", st.id, dialAddr, err)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	defer func() { _ = tcpConn.Close() }()

	if err := s.send(proto.TWsOpenOK, st.id, proto.MustJSON(proto.WsOpenOK{
		RelayMode: proto.WsRelayModeFramed,
	})); err != nil {
		return
	}

	var closeOnce sync.Once
	sendCloseToGateway := func(code int, reason string) {
		closeOnce.Do(func() {
			_ = s.send(proto.TWsClose, st.id, proto.MustJSON(proto.WsClose{Code: code, Reason: reason}))
			st.close()
		})
	}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				payload := make([]byte, 1+n)
				payload[0] = byte(websocket.BinaryMessage)
				copy(payload[1:], buf[:n])
				if err2 := s.send(proto.TWsA2C, st.id, payload); err2 != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					sendCloseToGateway(websocket.CloseNormalClosure, "upstream tcp closed")
				} else if errors.Is(err, net.ErrClosed) {
					return
				} else {
					log.Printf("[clockbridge-cli] upstream tcp read failed stream=%d target=%s err=%v", st.id, dialAddr, err)
					sendCloseToGateway(websocket.CloseGoingAway, "upstream tcp read failed")
				}
				return
			}
		}
	}()

	for {
		select {
		case <-st.closed:
			return
		case <-ctx.Done():
			return
		case f := <-st.rx:
			switch f.Type {
			case proto.TWsC2A:
				if len(f.Payload) <= 1 {
					continue
				}
				if _, err := tcpConn.Write(f.Payload[1:]); err != nil {
					log.Printf("[clockbridge-cli] upstream tcp write failed stream=%d target=%s err=%v", st.id, dialAddr, err)
					sendCloseToGateway(websocket.CloseGoingAway, "upstream tcp write failed")
					return
				}
			case proto.TWsClose:
				var c proto.WsClose
				_ = json.Unmarshal(f.Payload, &c)
				code := shared.SanitizeCloseCode(c.Code)
				reason := c.Reason
				log.Printf("[clockbridge-cli] gateway websocket close (transparent tcp) stream=%d code=%d reason=%q", st.id, code, reason)
				return
			case proto.TRst:
				log.Printf("[clockbridge-cli] gateway reset websocket stream=%d (transparent tcp)", st.id)
				return
			}
		}
	}
}

func (s *connState) handleWebSocketToTCP(ctx context.Context, st *stream, rs proto.ReqStart) {
	dialAddr := strings.TrimSpace(s.a.targetBase.Host)
	if dialAddr == "" {
		log.Printf("[clockbridge-cli] upstream tcp target host is empty stream=%d", st.id)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: "invalid tcp target: empty host"}))
		return
	}
	tcpConn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		log.Printf("[clockbridge-cli] upstream tcp dial failed stream=%d target=%s err=%v", st.id, dialAddr, err)
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	defer func() { _ = tcpConn.Close() }()

	if err := s.send(proto.TWsOpenOK, st.id, proto.MustJSON(proto.WsOpenOK{
		RelayMode: proto.WsRelayModeFramed,
	})); err != nil {
		return
	}

	var closeOnce sync.Once
	sendCloseToGateway := func(code int, reason string) {
		closeOnce.Do(func() {
			_ = s.send(proto.TWsClose, st.id, proto.MustJSON(proto.WsClose{Code: code, Reason: reason}))
			st.close()
		})
	}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				payload := make([]byte, 1+n)
				payload[0] = byte(websocket.BinaryMessage)
				copy(payload[1:], buf[:n])
				if err2 := s.send(proto.TWsA2C, st.id, payload); err2 != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					sendCloseToGateway(websocket.CloseNormalClosure, "upstream tcp closed")
				} else if errors.Is(err, net.ErrClosed) {
					// Closed by local teardown path (client closed/reset).
					return
				} else {
					log.Printf("[clockbridge-cli] upstream tcp read failed stream=%d target=%s err=%v", st.id, dialAddr, err)
					sendCloseToGateway(websocket.CloseGoingAway, "upstream tcp read failed")
				}
				return
			}
		}
	}()

	for {
		select {
		case <-st.closed:
			return
		case <-ctx.Done():
			return
		case f := <-st.rx:
			switch f.Type {
			case proto.TWsC2A:
				if len(f.Payload) <= 1 {
					continue
				}
				if _, err := tcpConn.Write(f.Payload[1:]); err != nil {
					log.Printf("[clockbridge-cli] upstream tcp write failed stream=%d target=%s err=%v", st.id, dialAddr, err)
					sendCloseToGateway(websocket.CloseGoingAway, "upstream tcp write failed")
					return
				}
			case proto.TWsClose:
				var c proto.WsClose
				_ = json.Unmarshal(f.Payload, &c)
				code := shared.SanitizeCloseCode(c.Code)
				reason := c.Reason
				log.Printf("[clockbridge-cli] gateway websocket close (tcp upstream) stream=%d code=%d reason=%q", st.id, code, reason)
				return
			case proto.TRst:
				log.Printf("[clockbridge-cli] gateway reset websocket stream=%d (tcp upstream)", st.id)
				return
			}
		}
	}
}

func (s *connState) closeAllStreams() {
	s.streamMu.Lock()
	list := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		list = append(list, st)
	}
	// Clear map to avoid delivering frames after teardown.
	s.streams = make(map[uint32]*stream)
	s.streamMu.Unlock()

	for _, st := range list {
		st.close()
	}
}
