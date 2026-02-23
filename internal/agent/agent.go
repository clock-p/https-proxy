package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
	"github.com/clock-p/clockbridge/internal/shared"
	"github.com/gorilla/websocket"
)

type Agent struct {
	registerURL         *url.URL
	registerDialAddr    string
	registerXToken      string
	registerBearerToken string
	targetBase          *url.URL
	wsReadLimit         int64

	httpClient    *http.Client
	httpTransport *http.Transport
}

type connState struct {
	a *Agent

	ws      *websocket.Conn
	writeMu sync.Mutex

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
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Agent{
		registerURL:         registerURL,
		registerDialAddr:    registerDialAddr,
		registerXToken:      registerXToken,
		registerBearerToken: registerBearerToken,
		targetBase:          targetBase,
		wsReadLimit:         shared.WSReadLimitBytes,
		httpTransport:       transport,
		httpClient: &http.Client{
			Transport: transport,
		},
	}
}

func (a *Agent) Run(ctx context.Context) error {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

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

		ws, _, err := dialer.DialContext(ctx, a.registerURL.String(), h)
		if err != nil {
			attempt++
			wait := reconnectBackoff(rnd, attempt)
			log.Printf("[clockbridge-cli] dial failed (attempt=%d) register_url=%s err=%v; retry in %s", attempt, a.registerURL.String(), err, wait)
			if !sleepWithContext(ctx, wait) {
				return ctx.Err()
			}
			continue
		}

		// Reset backoff after a successful connect.
		attempt = 0

		s := &connState{
			a:       a,
			ws:      ws,
			streams: make(map[uint32]*stream),
		}

		log.Printf("[clockbridge-cli] connected register_url=%s target=%s", a.registerURL.String(), a.targetBase.String())

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
		if code, text, ok := shared.CloseFromErr(err); ok && code == 4009 {
			// gateway: uuid already active; retrying will just spam logs.
			return fmt.Errorf("gateway closed: %d %s", code, text)
		}

		attempt++
		wait := reconnectBackoff(rnd, attempt)
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

func (s *connState) run(ctx context.Context) error {
	s.ws.SetPongHandler(func(appData string) error {
		_ = s.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	s.ws.SetReadLimit(s.a.wsReadLimit)
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
			_ = s.ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(2*time.Second))
			s.writeMu.Unlock()
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
			return err
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := proto.DecodeFrame(msg)
		if err != nil {
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
	return s.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (s *connState) deliverFrame(st *stream, f proto.Frame) bool {
	select {
	case <-st.closed:
		return false
	case st.rx <- f:
		return true
	default:
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
		_ = s.send(proto.TRst, st.id, []byte("missing req_start"))
		return
	}
	var rs proto.ReqStart
	if err := json.Unmarshal(f.Payload, &rs); err != nil {
		_ = s.send(proto.TRst, st.id, []byte("bad req_start"))
		return
	}

	if rs.IsWebSocket {
		s.handleWebSocket(ctx, st, rs)
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

func (s *connState) handleWebSocket(ctx context.Context, st *stream, rs proto.ReqStart) {
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
		_ = s.send(proto.TWsOpenErr, st.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	wsUp.SetReadLimit(s.a.wsReadLimit)
	defer func() { _ = wsUp.Close() }()

	sub := wsUp.Subprotocol()
	if err := s.send(proto.TWsOpenOK, st.id, proto.MustJSON(proto.WsOpenOK{Subprotocol: sub})); err != nil {
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
				_ = wsUp.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(2*time.Second))
				return
			case proto.TRst:
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
