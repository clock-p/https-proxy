package gateway

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/clock-p/clockbridge/internal/proto"
)

var ErrStreamTimeout = errors.New("stream recv timeout")

// HTTP response chunks are at most 32 KiB, so 640 queued chunks hold about 20 MiB.
const streamReceiveQueueFrames = 640

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
		rx:       make(chan proto.Frame, streamReceiveQueueFrames),
		closedCh: make(chan struct{}),
	}
}

func (s *stream) deliver(f proto.Frame) {
	select {
	case s.rx <- f:
		return
	case <-s.closedCh:
		return
	default:
		s.resetWithErr("stream rx overflow")
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

func (s *stream) resetWithErr(reason string) {
	err := errors.New(reason)
	s.closeOnce.Do(func() {
		log.Printf("[clockbridge-gateway] stream reset uuid=%s stream=%d reason=%s", s.c.uuid, s.id, reason)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.closedCh)
		s.c.forgetStream(s.id)
		s.sendReset(reason)
	})
}

func (s *stream) sendReset(reason string) {
	// The stream is already locally closed, so send the reset at the connection level.
	go func() {
		_ = s.c.writeFrame(proto.TRst, s.id, []byte(reason))
	}()
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
			return proto.Frame{}, s.closeErr()
		}
	}
	select {
	case f := <-s.rx:
		return f, nil
	case <-s.closedCh:
		return proto.Frame{}, s.closeErr()
	}
}

func (s *stream) closeErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return errors.New("stream closed")
}
