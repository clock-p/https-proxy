package agent

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/clock-p/clockbridge/internal/shared"
)

func TestProxyModeFromRegisterURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "nil url fallback", raw: "", want: shared.ProxyModeLegacyFramed},
		{name: "legacy from empty query", raw: "wss://register.example.com/register?uuid=u1", want: shared.ProxyModeLegacyFramed},
		{name: "transparent from query", raw: "wss://register.example.com/register?uuid=u1&proxy_mode=transparent_tunnel", want: shared.ProxyModeTransparentTunnel},
		{name: "invalid query fallback", raw: "wss://register.example.com/register?uuid=u1&proxy_mode=bad", want: shared.ProxyModeLegacyFramed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.raw == "" {
				if got := proxyModeFromRegisterURL(nil); got != tc.want {
					t.Fatalf("expected %q, got %q", tc.want, got)
				}
				return
			}
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("parse url failed: %v", err)
			}
			got := proxyModeFromRegisterURL(u)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestTargetKindFromRegisterURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "nil url fallback unknown", raw: "", want: shared.TargetKindUnknown},
		{name: "missing query fallback unknown", raw: "wss://register.example.com/register?uuid=u1", want: shared.TargetKindUnknown},
		{name: "ws target kind", raw: "wss://register.example.com/register?uuid=u1&target_kind=ws", want: shared.TargetKindWebSocket},
		{name: "tcp target kind", raw: "wss://register.example.com/register?uuid=u1&target_kind=tcp", want: shared.TargetKindTCP},
		{name: "invalid fallback unknown", raw: "wss://register.example.com/register?uuid=u1&target_kind=bad", want: shared.TargetKindUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.raw == "" {
				if got := targetKindFromRegisterURL(nil); got != tc.want {
					t.Fatalf("expected %q, got %q", tc.want, got)
				}
				return
			}
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("parse url failed: %v", err)
			}
			got := targetKindFromRegisterURL(u)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestShouldUseRawTransparentWS(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "ws", raw: "ws", want: true},
		{name: "uppercase ws", raw: "WS", want: true},
		{name: "unknown", raw: "unknown", want: false},
		{name: "empty", raw: "", want: false},
		{name: "tcp", raw: "tcp", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldUseRawTransparentWS(tc.raw)
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestNegotiatedProxyModeFromHandshake(t *testing.T) {
	cases := []struct {
		name     string
		resp     *http.Response
		fallback string
		want     string
	}{
		{name: "nil response uses fallback", resp: nil, fallback: shared.ProxyModeLegacyFramed, want: shared.ProxyModeLegacyFramed},
		{name: "no header uses fallback", resp: &http.Response{Header: http.Header{}}, fallback: shared.ProxyModeTransparentTunnel, want: shared.ProxyModeTransparentTunnel},
		{
			name: "valid header overrides fallback",
			resp: &http.Response{Header: http.Header{
				shared.HeaderProxyMode: []string{shared.ProxyModeLegacyFramed},
			}},
			fallback: shared.ProxyModeTransparentTunnel,
			want:     shared.ProxyModeLegacyFramed,
		},
		{
			name: "invalid header fallback",
			resp: &http.Response{Header: http.Header{
				shared.HeaderProxyMode: []string{"bad"},
			}},
			fallback: shared.ProxyModeTransparentTunnel,
			want:     shared.ProxyModeTransparentTunnel,
		},
		{
			name:     "empty fallback defaults legacy",
			resp:     nil,
			fallback: "",
			want:     shared.ProxyModeLegacyFramed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := negotiatedProxyModeFromHandshake(tc.resp, tc.fallback)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
