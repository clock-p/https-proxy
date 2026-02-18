package gateway

import (
	"errors"
	"sync"
	"time"

	"github.com/clock-p/https-proxy/internal/proto"
)

var ErrStreamTimeout = errors.New("stream recv timeout")

type stream struct {
	id uint32
	c  *agentConn

	rx chan proto.Frame

	closeOnce sync.Once
	closedCh  chan struct{}

	mu  sync.Mutex
	err error
}

func newStream(id uint32, c *agentConn) *stream {
	return &stream{
		id:       id,
		c:        c,
		rx:       make(chan proto.Frame, 64),
		closedCh: make(chan struct{}),
	}
}

func (s *stream) deliver(f proto.Frame) {
	select {
	case s.rx <- f:
	case <-s.closedCh:
	}
}

func (s *stream) closeWithErr(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.closedCh)
		s.c.forgetStream(s.id)
	})
}

func (s *stream) send(t byte, payload []byte) error {
	select {
	case <-s.closedCh:
		return errors.New("stream closed")
	default:
		return s.c.writeFrame(t, s.id, payload)
	}
}

func (s *stream) recv(deadline time.Duration) (proto.Frame, error) {
	if deadline > 0 {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		select {
		case f := <-s.rx:
			return f, nil
		case <-timer.C:
			return proto.Frame{}, ErrStreamTimeout
		case <-s.closedCh:
			return proto.Frame{}, errors.New("stream closed")
		}
	}
	select {
	case f := <-s.rx:
		return f, nil
	case <-s.closedCh:
		return proto.Frame{}, errors.New("stream closed")
	}
}
