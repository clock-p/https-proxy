package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clock-p/clockbridge/internal/shared"
)

func TestApplyQueryBearerAuthAccessToken(t *testing.T) {
	h := http.Header{}
	rawQuery := "access_token=abc123&keep=1"

	applyQueryBearerAuth(h, &rawQuery)

	if got, want := h.Get("Authorization"), "Bearer abc123"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if got, want := rawQuery, "keep=1"; got != want {
		t.Fatalf("rawQuery = %q, want %q", got, want)
	}
}

func TestApplyQueryBearerAuthAuthorization(t *testing.T) {
	h := http.Header{}
	rawQuery := "authorization=Bearer+xyz&keep=1"

	applyQueryBearerAuth(h, &rawQuery)

	if got, want := h.Get("Authorization"), "Bearer xyz"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if got, want := rawQuery, "keep=1"; got != want {
		t.Fatalf("rawQuery = %q, want %q", got, want)
	}
}

func TestApplyQueryBearerAuthBareAuthorization(t *testing.T) {
	h := http.Header{}
	rawQuery := "authorization=xyz"

	applyQueryBearerAuth(h, &rawQuery)

	if got, want := h.Get("Authorization"), "Bearer xyz"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if rawQuery != "" {
		t.Fatalf("rawQuery = %q, want empty", rawQuery)
	}
}

func TestStatusIncludesAgentDiagnostics(t *testing.T) {
	s := NewServerFromEnv()
	c := newAgentConn(
		"diag",
		shared.ProxyModeLegacyFramed,
		shared.ProxyModeLegacyFramed,
		"legacy_requested",
		shared.TargetKindUnknown,
		nil,
		0,
	)
	st := newStream(1, c)
	c.streams[st.id] = st
	s.agentByUUID[c.uuid] = c

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status response failed: %v", err)
	}
	assertNestedNumber(t, payload, "active_streams", "diag", 1)
	assertNestedNumber(t, payload, "agent_age_seconds", "diag", 0)
	assertNestedNumber(t, payload, "agent_idle_seconds", "diag", 0)
}

func assertNestedNumber(t *testing.T, payload map[string]any, key string, nestedKey string, want float64) {
	t.Helper()
	values, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("%s missing or not object: %#v", key, payload[key])
	}
	got, ok := values[nestedKey].(float64)
	if !ok {
		t.Fatalf("%s[%s] missing or not number: %#v", key, nestedKey, values[nestedKey])
	}
	if got != want {
		t.Fatalf("%s[%s] = %v, want %v", key, nestedKey, got, want)
	}
}
