package gateway

import (
	"net/http"
	"testing"
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
