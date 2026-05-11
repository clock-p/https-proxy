package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clock-p/clockbridge/internal/agent"
	"github.com/clock-p/clockbridge/internal/shared"
	"github.com/gorilla/websocket"
)

type gatewayStatusSnapshot struct {
	Count              int               `json:"count"`
	UUIDs              []string          `json:"uuids"`
	ProxyModes         map[string]string `json:"proxy_modes"`
	ProxyModesReq      map[string]string `json:"proxy_modes_requested"`
	ProxyModeReasons   map[string]string `json:"proxy_mode_reasons"`
	TargetKinds        map[string]string `json:"target_kinds"`
	ProxyModePolicyRaw map[string]any    `json:"proxy_mode_policy"`
}

func TestWebSocketTCPModeSelection(t *testing.T) {
	cases := []struct {
		name               string
		transparentEnabled bool
		allowUUIDs         string
		forceLegacy        bool
		wantMode           string
		wantReason         string
	}{
		{
			name:               "transparent enabled and whitelisted",
			transparentEnabled: true,
			allowUUIDs:         "u1",
			forceLegacy:        false,
			wantMode:           shared.ProxyModeTransparentTunnel,
			wantReason:         "transparent_allowed",
		},
		{
			name:               "transparent requested but disabled fallback legacy",
			transparentEnabled: false,
			allowUUIDs:         "",
			forceLegacy:        false,
			wantMode:           shared.ProxyModeLegacyFramed,
			wantReason:         "transparent_disabled",
		},
		{
			name:               "transparent requested but force legacy",
			transparentEnabled: true,
			allowUUIDs:         "u1",
			forceLegacy:        true,
			wantMode:           shared.ProxyModeLegacyFramed,
			wantReason:         "force_legacy_mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const uuid = "u1"
			echoAddr, closeEcho := startTCPEchoServer(t)
			defer closeEcho()

			t.Setenv("HTTPS_PROXY_TRANSPARENT_ENABLED", boolString(tc.transparentEnabled))
			t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS", tc.allowUUIDs)
			t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES", "")
			t.Setenv("HTTPS_PROXY_FORCE_LEGACY_MODE", boolString(tc.forceLegacy))
			t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")

			s := NewServerFromEnv()
			ts := httptest.NewServer(s)
			defer ts.Close()

			registerURL, err := buildRegisterURLForTest(ts.URL, uuid, shared.ProxyModeTransparentTunnel, shared.TargetKindTCP)
			if err != nil {
				t.Fatalf("build register url failed: %v", err)
			}
			targetURL, err := url.Parse("tcp://" + echoAddr)
			if err != nil {
				t.Fatalf("parse target url failed: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agentErrCh := make(chan error, 1)
			go func() {
				agentErrCh <- agent.New(registerURL, "", "", "", targetURL).Run(ctx)
			}()

			waitForAgentOnline(t, ts.URL, uuid, 5*time.Second)

			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + uuid + "/ssh"
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				t.Fatalf("dial ws failed: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			payload := []byte("hello-mode-selection")
			if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				_ = conn.Close()
				t.Fatalf("write ws failed: %v", err)
			}
			msgType, got, err := conn.ReadMessage()
			if err != nil {
				_ = conn.Close()
				t.Fatalf("read ws failed: %v", err)
			}
			_ = conn.Close()
			if msgType != websocket.BinaryMessage {
				t.Fatalf("expected binary message, got type=%d", msgType)
			}
			if string(got) != string(payload) {
				t.Fatalf("payload mismatch want=%q got=%q", payload, got)
			}

			status := fetchStatus(t, ts.URL)
			if status.ProxyModes[uuid] != tc.wantMode {
				t.Fatalf("expected mode=%q, got=%q", tc.wantMode, status.ProxyModes[uuid])
			}
			if status.ProxyModesReq[uuid] != shared.ProxyModeTransparentTunnel {
				t.Fatalf("expected requested mode=%q, got=%q", shared.ProxyModeTransparentTunnel, status.ProxyModesReq[uuid])
			}
			if status.ProxyModeReasons[uuid] != tc.wantReason {
				t.Fatalf("expected reason=%q, got=%q", tc.wantReason, status.ProxyModeReasons[uuid])
			}

			cancel()
			select {
			case err := <-agentErrCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("agent exited with unexpected error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timeout waiting for agent shutdown")
			}
		})
	}
}

func TestTransparentRelayModeMismatchFailsFast(t *testing.T) {
	const uuid = "u1"
	echoAddr, closeEcho := startTCPEchoServer(t)
	defer closeEcho()

	t.Setenv("HTTPS_PROXY_TRANSPARENT_ENABLED", "true")
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS", uuid)
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES", "")
	t.Setenv("HTTPS_PROXY_FORCE_LEGACY_MODE", "false")
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")

	s := NewServerFromEnv()
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Intentional mismatch:
	// - register says target_kind=ws (gateway will pick raw path)
	// - actual target is tcp://... (agent will pick framed path)
	registerURL, err := buildRegisterURLForTest(ts.URL, uuid, shared.ProxyModeTransparentTunnel, shared.TargetKindWebSocket)
	if err != nil {
		t.Fatalf("build register url failed: %v", err)
	}
	targetURL, err := url.Parse("tcp://" + echoAddr)
	if err != nil {
		t.Fatalf("parse target url failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.New(registerURL, "", "", "", targetURL).Run(ctx)
	}()
	waitForAgentOnline(t, ts.URL, uuid, 5*time.Second)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + uuid + "/ssh"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected websocket dial to fail on relay mismatch")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("expected status 502, got %d (err=%v)", status, err)
	}

	cancel()
	select {
	case err := <-agentErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("agent exited with unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for agent shutdown")
	}
}

func TestTransparentWSTargetUnknownKindFallbackFramed(t *testing.T) {
	const uuid = "u1"
	upstreamURL, closeWSEcho := startWSEchoServer(t)
	defer closeWSEcho()

	t.Setenv("HTTPS_PROXY_TRANSPARENT_ENABLED", "true")
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS", uuid)
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES", "")
	t.Setenv("HTTPS_PROXY_FORCE_LEGACY_MODE", "false")
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")

	s := NewServerFromEnv()
	ts := httptest.NewServer(s)
	defer ts.Close()

	// target_kind omitted -> unknown. Gateway selects framed transparent path.
	registerURL, err := buildRegisterURLForTest(ts.URL, uuid, shared.ProxyModeTransparentTunnel, "")
	if err != nil {
		t.Fatalf("build register url failed: %v", err)
	}
	targetURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse target url failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.New(registerURL, "", "", "", targetURL).Run(ctx)
	}()
	waitForAgentOnline(t, ts.URL, uuid, 5*time.Second)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + uuid + "/events"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws failed: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	payload := []byte("ws-fallback-ok")
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write ws failed: %v", err)
	}
	mt, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws failed: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("expected binary message, got type=%d", mt)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch want=%q got=%q", payload, got)
	}

	cancel()
	select {
	case err := <-agentErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("agent exited with unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for agent shutdown")
	}
}

func TestTransparentWSTargetRawSuccessAndStatusFields(t *testing.T) {
	const uuid = "u1"
	upstreamURL, closeWSEcho := startWSEchoServerWithSubprotocol(t, []string{"clock-v1"})
	defer closeWSEcho()

	t.Setenv("HTTPS_PROXY_TRANSPARENT_ENABLED", "true")
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS", uuid)
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES", "ssh-")
	t.Setenv("HTTPS_PROXY_FORCE_LEGACY_MODE", "false")
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")

	s := NewServerFromEnv()
	ts := httptest.NewServer(s)
	defer ts.Close()

	registerURL, err := buildRegisterURLForTest(ts.URL, uuid, shared.ProxyModeTransparentTunnel, shared.TargetKindWebSocket)
	if err != nil {
		t.Fatalf("build register url failed: %v", err)
	}
	targetURL, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse target url failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- agent.New(registerURL, "", "", "", targetURL).Run(ctx)
	}()
	waitForAgentOnline(t, ts.URL, uuid, 5*time.Second)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + uuid + "/events"
	dialer := websocket.Dialer{
		Subprotocols:      []string{"clock-v1"},
		EnableCompression: true,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws failed: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if conn.Subprotocol() != "clock-v1" {
		t.Fatalf("expected negotiated subprotocol %q, got %q", "clock-v1", conn.Subprotocol())
	}

	payload := []byte("raw-ws-ok")
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write ws failed: %v", err)
	}
	mt, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws failed: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("expected text message, got type=%d", mt)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch want=%q got=%q", payload, got)
	}

	status := fetchStatus(t, ts.URL)
	if status.ProxyModes[uuid] != shared.ProxyModeTransparentTunnel {
		t.Fatalf("expected mode=%q, got=%q", shared.ProxyModeTransparentTunnel, status.ProxyModes[uuid])
	}
	if status.ProxyModesReq[uuid] != shared.ProxyModeTransparentTunnel {
		t.Fatalf("expected requested mode=%q, got=%q", shared.ProxyModeTransparentTunnel, status.ProxyModesReq[uuid])
	}
	if status.TargetKinds[uuid] != shared.TargetKindWebSocket {
		t.Fatalf("expected target kind=%q, got=%q", shared.TargetKindWebSocket, status.TargetKinds[uuid])
	}

	if got := policyBool(t, status.ProxyModePolicyRaw, "transparent_enabled"); !got {
		t.Fatalf("expected transparent_enabled=true, got=%v", got)
	}
	if got := policyBool(t, status.ProxyModePolicyRaw, "force_legacy_mode"); got {
		t.Fatalf("expected force_legacy_mode=false, got=%v", got)
	}
	allowUUIDs := policyStringSlice(t, status.ProxyModePolicyRaw, "allow_uuids")
	if !containsString(allowUUIDs, uuid) {
		t.Fatalf("expected allow_uuids to contain %q, got=%v", uuid, allowUUIDs)
	}
	allowPrefixes := policyStringSlice(t, status.ProxyModePolicyRaw, "allow_prefixes")
	if !containsString(allowPrefixes, "ssh-") {
		t.Fatalf("expected allow_prefixes to contain %q, got=%v", "ssh-", allowPrefixes)
	}

	cancel()
	select {
	case err := <-agentErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("agent exited with unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for agent shutdown")
	}
}

func TestStatusWithoutAgentsReportsPolicy(t *testing.T) {
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ENABLED", "true")
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS", "u1,u2")
	t.Setenv("HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES", "ssh-,ws-")
	t.Setenv("HTTPS_PROXY_FORCE_LEGACY_MODE", "false")
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "")

	s := NewServerFromEnv()
	ts := httptest.NewServer(s)
	defer ts.Close()

	status := fetchStatus(t, ts.URL)
	if status.Count != 0 {
		t.Fatalf("expected count=0, got=%d", status.Count)
	}
	if len(status.UUIDs) != 0 {
		t.Fatalf("expected empty uuids, got=%v", status.UUIDs)
	}
	if len(status.ProxyModes) != 0 {
		t.Fatalf("expected empty proxy_modes, got=%v", status.ProxyModes)
	}
	if len(status.TargetKinds) != 0 {
		t.Fatalf("expected empty target_kinds, got=%v", status.TargetKinds)
	}
	if got := policyBool(t, status.ProxyModePolicyRaw, "transparent_enabled"); !got {
		t.Fatalf("expected transparent_enabled=true, got=%v", got)
	}
	if got := policyBool(t, status.ProxyModePolicyRaw, "force_legacy_mode"); got {
		t.Fatalf("expected force_legacy_mode=false, got=%v", got)
	}

	allowUUIDs := policyStringSlice(t, status.ProxyModePolicyRaw, "allow_uuids")
	if !containsString(allowUUIDs, "u1") || !containsString(allowUUIDs, "u2") {
		t.Fatalf("expected allow_uuids contain [u1 u2], got=%v", allowUUIDs)
	}
	allowPrefixes := policyStringSlice(t, status.ProxyModePolicyRaw, "allow_prefixes")
	if !containsString(allowPrefixes, "ssh-") || !containsString(allowPrefixes, "ws-") {
		t.Fatalf("expected allow_prefixes contain [ssh- ws-], got=%v", allowPrefixes)
	}
}

func startTCPEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start tcp echo server failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	closeFn := func() {
		close(done)
		_ = ln.Close()
	}
	return ln.Addr().String(), closeFn
}

func fetchStatus(t *testing.T, baseURL string) gatewayStatusSnapshot {
	t.Helper()
	resp, err := http.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("get /status failed: %v", err)
	}
	defer resp.Body.Close()
	var out gatewayStatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /status failed: %v", err)
	}
	return out
}

func waitForAgentOnline(t *testing.T, baseURL string, uuid string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := fetchStatus(t, baseURL)
		if _, ok := status.ProxyModes[uuid]; ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent %q did not come online before timeout", uuid)
}

func buildRegisterURLForTest(httpBaseURL string, uuid string, mode string, targetKind string) (*url.URL, error) {
	parsed, err := url.Parse(httpBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse http base url: %w", err)
	}
	parsed.Scheme = "ws"
	parsed.Path = "/register"
	q := parsed.Query()
	q.Set("uuid", uuid)
	q.Set("proxy_mode", mode)
	if strings.TrimSpace(targetKind) != "" {
		q.Set("target_kind", targetKind)
	}
	parsed.RawQuery = q.Encode()
	return parsed, nil
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func startWSEchoServer(t *testing.T) (string, func()) {
	return startWSEchoServerWithOptions(t, nil, false)
}

func startWSEchoServerWithSubprotocol(t *testing.T, subprotocols []string) (string, func()) {
	return startWSEchoServerWithOptions(t, subprotocols, true)
}

func startWSEchoServerWithOptions(t *testing.T, subprotocols []string, enableCompression bool) (string, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		CheckOrigin:       func(r *http.Request) bool { return true },
		Subprotocols:      append([]string(nil), subprotocols...),
		EnableCompression: enableCompression,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	})
	ts := httptest.NewServer(mux)
	return "ws" + strings.TrimPrefix(ts.URL, "http"), ts.Close
}

func policyBool(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing policy key %q", key)
	}
	v, ok := raw.(bool)
	if !ok {
		t.Fatalf("policy %q is not bool, got %T", key, raw)
	}
	return v
}

func policyStringSlice(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing policy key %q", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("policy %q is not []any, got %T", key, raw)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("policy %q item is not string, got %T", key, item)
		}
		out = append(out, s)
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
