package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clock-p/clockbridge/internal/agent"
	"github.com/clock-p/clockbridge/internal/shared"
	"github.com/gorilla/websocket"
)

// Exercise every read limit, including the framed gateway/agent connection.
func TestWebSocketMessageLimitRoundTrip(t *testing.T) {
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")
	echoURL, closeEcho := startWSEchoServer(t)
	defer closeEcho()
	target, err := url.Parse("http" + strings.TrimPrefix(echoURL, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServerFromEnv())
	defer srv.Close()
	register, err := buildRegisterURLForTest(srv.URL, "large-message", shared.ProxyModeLegacyFramed, shared.TargetKindHTTP)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agent.New(register, "", "", "", target).Run(ctx) }()
	waitForAgentOnline(t, srv.URL, "large-message", 5*time.Second)
	for _, size := range []int{12 << 20, 40 << 20, (40 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/large-message/echo", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			payload := bytes.Repeat([]byte("x"), size)
			writeErr := conn.WriteMessage(websocket.BinaryMessage, payload)
			mt, got, readErr := conn.ReadMessage()
			if size > 40<<20 {
				if !websocket.IsCloseError(readErr, websocket.CloseMessageTooBig) {
					t.Fatalf("expected message-too-big close; write=%v read=%v", writeErr, readErr)
				}
				return
			}
			if writeErr != nil || readErr != nil {
				t.Fatalf("round trip failed: write=%v read=%v", writeErr, readErr)
			}
			if mt != websocket.BinaryMessage || !bytes.Equal(got, payload) {
				t.Fatalf("echo mismatch: type=%d bytes=%d want=%d", mt, len(got), size)
			}
		})
	}
}
