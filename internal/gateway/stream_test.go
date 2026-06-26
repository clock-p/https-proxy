package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
	"github.com/gorilla/websocket"
)

func TestStreamDeliverOverflowResetsAndClosesStream(t *testing.T) {
	clientConn, peerConn := newTestWebSocketPair(t)
	defer clientConn.Close()
	defer peerConn.Close()

	c := &agentConn{
		uuid:     "overflow-test",
		ws:       clientConn,
		streams:  make(map[uint32]*stream),
		closedCh: make(chan struct{}),
	}
	st := newStream(1, c)
	c.streams[st.id] = st

	for i := 0; i < cap(st.rx); i++ {
		st.rx <- proto.Frame{Type: proto.TResData, Stream: st.id}
	}

	done := make(chan struct{})
	go func() {
		st.deliver(proto.Frame{Type: proto.TResData, Stream: st.id})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliver blocked on a full stream buffer")
	}

	select {
	case <-st.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not closed after overflow")
	}

	if got := c.getStream(st.id); got != nil {
		t.Fatal("stream was not removed from agent connection after overflow")
	}

	if err := peerConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline failed: %v", err)
	}
	mt, msg, err := peerConn.ReadMessage()
	if err != nil {
		t.Fatalf("read reset frame failed: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("expected binary reset frame, got message type=%d", mt)
	}
	f, err := proto.DecodeFrame(msg)
	if err != nil {
		t.Fatalf("decode reset frame failed: %v", err)
	}
	if f.Type != proto.TRst {
		t.Fatalf("expected reset frame type=%d, got=%d", proto.TRst, f.Type)
	}
	if f.Stream != st.id {
		t.Fatalf("expected reset frame stream=%d, got=%d", st.id, f.Stream)
	}
	if string(f.Payload) != "stream rx overflow" {
		t.Fatalf("unexpected reset payload %q", string(f.Payload))
	}
}

func TestAgentConnCloseWithErrDoesNotDeadlockWithActiveStream(t *testing.T) {
	c := &agentConn{
		uuid:     "close-test",
		streams:  make(map[uint32]*stream),
		closedCh: make(chan struct{}),
	}
	st := newStream(1, c)
	c.streams[st.id] = st

	done := make(chan struct{})
	go func() {
		c.closeWithErr(errors.New("test close"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent connection close deadlocked with an active stream")
	}

	select {
	case <-st.closedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("active stream was not closed")
	}

	if got := c.getStream(st.id); got != nil {
		t.Fatal("stream was not removed after agent connection close")
	}
}

func TestAgentConnNewStreamFailsAfterClose(t *testing.T) {
	c := &agentConn{
		uuid:     "closed-agent-test",
		streams:  make(map[uint32]*stream),
		closedCh: make(chan struct{}),
	}

	c.closeWithErr(errors.New("test close"))

	if st, err := c.newStream(); err == nil {
		t.Fatalf("expected newStream to fail after agent close, got stream=%v", st)
	}
}

func newTestWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	serverConnCh := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		serverConnCh <- conn
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test websocket failed: %v", err)
	}

	select {
	case err := <-errCh:
		clientConn.Close()
		t.Fatalf("upgrade test websocket failed: %v", err)
	case serverConn := <-serverConnCh:
		return clientConn, serverConn
	case <-time.After(2 * time.Second):
		clientConn.Close()
		t.Fatal("timed out waiting for test websocket server connection")
	}

	return nil, nil
}
