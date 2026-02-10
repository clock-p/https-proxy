package shared

import (
	"errors"

	"github.com/gorilla/websocket"
)

const WSReadLimitBytes int64 = 10 << 20

func CloseFromErr(err error) (int, string, bool) {
	if err == nil {
		return 0, "", false
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code, ce.Text, true
	}
	return 0, "", false
}

func SanitizeCloseCode(code int) int {
	if code == 0 {
		return websocket.CloseNormalClosure
	}
	if code < 1000 || code >= 5000 {
		return websocket.CloseNormalClosure
	}
	switch code {
	case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseTLSHandshake:
		return websocket.CloseNormalClosure
	}
	return code
}
