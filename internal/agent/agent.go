package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/clock-p/https-proxy/internal/proto"
	"github.com/clock-p/https-proxy/internal/shared"
	"github.com/gorilla/websocket"
)

type Agent struct {
	registerURL *url.URL
	token       string
	targetBase  *url.URL
	wsReadLimit int64

	ws      *websocket.Conn
	writeMu sync.Mutex

	streamMu sync.Mutex
	streams  map[uint32]*stream

	httpClient    *http.Client
	httpTransport *http.Transport
}

type stream struct {
	id        uint32
	rx        chan proto.Frame
	closed    chan struct{}
	closeOnce sync.Once
}

func New(registerURL *url.URL, token string, targetBase *url.URL) *Agent {
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
		registerURL:   registerURL,
		token:         token,
		targetBase:    targetBase,
		wsReadLimit:   shared.WSReadLimitBytes,
		streams:       make(map[uint32]*stream),
		httpTransport: transport,
		httpClient: &http.Client{
			Transport: transport,
		},
	}
}

func (a *Agent) Run(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	h := http.Header{}
	h.Set("X-Token", a.token)

	ws, _, err := dialer.DialContext(ctx, a.registerURL.String(), h)
	if err != nil {
		return err
	}
	a.ws = ws
	defer func() { _ = ws.Close() }()
	if a.httpTransport != nil {
		defer a.httpTransport.CloseIdleConnections()
	}

	log.Printf("[https-proxy-agent] connected register_url=%s target=%s", a.registerURL.String(), a.targetBase.String())

	ws.SetPongHandler(func(appData string) error {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	ws.SetReadLimit(a.wsReadLimit)
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))

	go a.pingLoop(ctx)
	return a.readLoop(ctx)
}

func (a *Agent) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.writeMu.Lock()
			_ = a.ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(2*time.Second))
			a.writeMu.Unlock()
		}
	}
}

func (a *Agent) readLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		mt, msg, err := a.ws.ReadMessage()
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
		s := a.getOrCreateStream(ctx, f.Stream)
		_ = a.deliverFrame(s, f)
	}
}

func (a *Agent) getOrCreateStream(ctx context.Context, id uint32) *stream {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	if s := a.streams[id]; s != nil {
		return s
	}
	s := &stream{id: id, rx: make(chan proto.Frame, 128), closed: make(chan struct{})}
	a.streams[id] = s
	go a.handleStream(ctx, s)
	return s
}

func (a *Agent) forgetStream(id uint32) {
	a.streamMu.Lock()
	s := a.streams[id]
	delete(a.streams, id)
	a.streamMu.Unlock()
	if s != nil {
		s.close()
	}
}

func (s *stream) close() {
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func (a *Agent) send(t byte, stream uint32, payload []byte) error {
	b := proto.EncodeFrame(t, stream, payload)
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (a *Agent) deliverFrame(s *stream, f proto.Frame) bool {
	select {
	case <-s.closed:
		return false
	case s.rx <- f:
		return true
	default:
		_ = a.send(proto.TRst, s.id, []byte("stream rx overflow"))
		a.forgetStream(s.id)
		return false
	}
}

func (a *Agent) handleStream(ctx context.Context, s *stream) {
	defer a.forgetStream(s.id)

	var f proto.Frame
	select {
	case <-s.closed:
		return
	case f = <-s.rx:
	}
	if f.Type != proto.TReqStart {
		_ = a.send(proto.TRst, s.id, []byte("missing req_start"))
		return
	}
	var rs proto.ReqStart
	if err := json.Unmarshal(f.Payload, &rs); err != nil {
		_ = a.send(proto.TRst, s.id, []byte("bad req_start"))
		return
	}

	if rs.IsWebSocket {
		a.handleWebSocket(ctx, s, rs)
		return
	}
	a.handleHTTP(ctx, s, rs)
}

func (a *Agent) handleHTTP(ctx context.Context, s *stream, rs proto.ReqStart) {
	pr, pw := io.Pipe()
	bodyClosed := make(chan struct{})
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, rs.Method, "", pr)
	if err != nil {
		_ = a.send(proto.TRst, s.id, []byte("bad request"))
		return
	}

	go func() {
		defer close(bodyClosed)
		defer func() { _ = pw.Close() }()
		for {
			select {
			case <-s.closed:
				cancel()
				return
			case <-reqCtx.Done():
				return
			case f := <-s.rx:
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

	target := shared.JoinURL(a.targetBase, rs.Path)
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
			_ = a.send(proto.TResStart, s.id, proto.MustJSON(proto.ResStart{Status: code, Header: h, Interim: true}))
			return nil
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := a.httpClient.Do(req)
	if err != nil {
		_ = a.send(proto.TResStart, s.id, proto.MustJSON(proto.ResStart{Status: http.StatusBadGateway}))
		_ = a.send(proto.TResEnd, s.id, nil)
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
	if err := a.send(proto.TResStart, s.id, proto.MustJSON(resStart)); err != nil {
		cancel()
		return
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if err2 := a.send(proto.TResData, s.id, buf[:n]); err2 != nil {
				cancel()
				return
			}
		}
		if err != nil {
			break
		}
	}

	if len(resp.Trailer) > 0 {
		if err := a.send(proto.TResTrailer, s.id, proto.MustJSON(map[string][]string(resp.Trailer))); err != nil {
			cancel()
			return
		}
	}
	_ = a.send(proto.TResEnd, s.id, nil)

	<-bodyClosed
}

func (a *Agent) handleWebSocket(ctx context.Context, s *stream, rs proto.ReqStart) {
	targetHTTP := shared.JoinURL(a.targetBase, rs.Path)
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
		_ = a.send(proto.TWsOpenErr, s.id, proto.MustJSON(proto.WsOpenErr{Message: err.Error()}))
		return
	}
	wsUp.SetReadLimit(a.wsReadLimit)
	defer func() { _ = wsUp.Close() }()

	sub := wsUp.Subprotocol()
	if err := a.send(proto.TWsOpenOK, s.id, proto.MustJSON(proto.WsOpenOK{Subprotocol: sub})); err != nil {
		return
	}

	var closeOnce sync.Once
	sendCloseToGateway := func(code int, reason string) {
		closeOnce.Do(func() {
			_ = a.send(proto.TWsClose, s.id, proto.MustJSON(proto.WsClose{Code: code, Reason: reason}))
			s.close()
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
			if err := a.send(proto.TWsA2C, s.id, payload); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-s.closed:
			return
		case <-ctx.Done():
			return
		case f := <-s.rx:
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
