package proto

import (
	"encoding/binary"
	"errors"
)

const headerBytes = 1 + 4 + 4

type Frame struct {
	Type    byte
	Stream  uint32
	Payload []byte
}

func EncodeFrame(t byte, stream uint32, payload []byte) []byte {
	out := make([]byte, headerBytes+len(payload))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], stream)
	binary.BigEndian.PutUint32(out[5:9], uint32(len(payload)))
	copy(out[9:], payload)
	return out
}

func DecodeFrame(b []byte) (Frame, error) {
	if len(b) < headerBytes {
		return Frame{}, errors.New("frame too short")
	}
	t := b[0]
	stream := binary.BigEndian.Uint32(b[1:5])
	n := binary.BigEndian.Uint32(b[5:9])
	if int(n) < 0 || len(b) != headerBytes+int(n) {
		return Frame{}, errors.New("frame length mismatch")
	}
	payload := b[9:]
	return Frame{Type: t, Stream: stream, Payload: payload}, nil
}
