package gateway

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
	"github.com/gorilla/websocket"
)

type agentConn struct {
	uuid string
	ws   *websocket.Conn

	writeMu sync.Mutex

	nextStream atomic.Uint32

	streamMu   sync.Mutex
	streams    map[uint32]*stream
	maxStreams int

	closedCh  chan struct{}
	closeOnce sync.Once

	bornAt              time.Time
	lastActiveMonoNanos atomic.Int64
	closeReasonVal      atomic.Value
}

func newAgentConn(uuid string, ws *websocket.Conn, maxStreams int) *agentConn {
	now := time.Now()
	c := &agentConn{
		uuid:       uuid,
		ws:         ws,
		streams:    make(map[uint32]*stream),
		maxStreams: maxStreams,
		closedCh:   make(chan struct{}),
		bornAt:     now,
	}
	c.markActive()
	return c
}

func (c *agentConn) closeWithErr(err error) {
	c.closeOnce.Do(func() {
		reason := "connection closed"
		if err != nil {
			reason = err.Error()
		}
		c.closeReasonVal.Store(reason)
		close(c.closedCh)
		c.streamMu.Lock()
		for _, s := range c.streams {
			s.closeWithErr(errors.New("agent disconnected"))
		}
		c.streams = make(map[uint32]*stream)
		c.streamMu.Unlock()
	})
}

func (c *agentConn) markActive() {
	c.lastActiveMonoNanos.Store(time.Since(c.bornAt).Nanoseconds())
}

func (c *agentConn) idleFor() time.Duration {
	last := time.Duration(c.lastActiveMonoNanos.Load())
	idle := time.Since(c.bornAt) - last
	if idle < 0 {
		return 0
	}
	return idle
}

func (c *agentConn) closeReason() string {
	v := c.closeReasonVal.Load()
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "connection closed"
}

func (c *agentConn) newStream() (*stream, error) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.maxStreams > 0 && len(c.streams) >= c.maxStreams {
		return nil, errors.New("too many streams")
	}
	id := c.nextStream.Add(1)
	if id == 0 {
		id = c.nextStream.Add(1)
	}
	s := newStream(uint32(id), c)
	c.streams[s.id] = s
	return s, nil
}

func (c *agentConn) forgetStream(id uint32) {
	c.streamMu.Lock()
	delete(c.streams, id)
	c.streamMu.Unlock()
}

func (c *agentConn) getStream(id uint32) *stream {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.streams[id]
}

func (c *agentConn) writeFrame(t byte, stream uint32, payload []byte) error {
	b := proto.EncodeFrame(t, stream, payload)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (c *agentConn) readLoop() {
	defer c.closeWithErr(errors.New("readLoop exit"))

	c.ws.SetPingHandler(func(appData string) error {
		c.markActive()
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		return c.ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(2*time.Second))
	})

	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		c.markActive()
		if mt != websocket.BinaryMessage {
			continue
		}
		f, err := proto.DecodeFrame(msg)
		if err != nil {
			continue
		}
		s := c.getStream(f.Stream)
		if s == nil {
			continue
		}
		s.deliver(f)
	}
}
