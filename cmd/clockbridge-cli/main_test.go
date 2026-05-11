package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/clock-p/clockbridge/internal/shared"
)

func TestParseRemoteTargetURL(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string
		wantErrPart string
	}{
		{name: "http ok", raw: "http://127.0.0.1:19090/", want: "http://127.0.0.1:19090/"},
		{name: "https ok", raw: "https://demo.example.com/base", want: "https://demo.example.com/base"},
		{name: "tcp ok", raw: "tcp://127.0.0.1:22", want: "tcp://127.0.0.1:22"},
		{name: "ws ok", raw: "ws://127.0.0.1:19090/ws", want: "ws://127.0.0.1:19090/ws"},
		{name: "wss ok", raw: "wss://demo.example.com/tunnel", want: "wss://demo.example.com/tunnel"},
		{name: "tcp ipv6 ok", raw: "tcp://[fd00::1]:2222", want: "tcp://[fd00::1]:2222"},
		{name: "tcp missing port", raw: "tcp://127.0.0.1", wantErrPart: "tcp target requires explicit port"},
		{name: "tcp with path", raw: "tcp://127.0.0.1:22/ssh", wantErrPart: "tcp target must not include path"},
		{name: "unsupported scheme", raw: "udp://127.0.0.1:53", wantErrPart: "only http/https/tcp/ws/wss are supported"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRemoteTargetURL(tc.raw, "remote target URL")
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got.String())
			}
		})
	}
}

func mustBuildRegisterURL(t *testing.T, rawHost string) string {
	t.Helper()
	u, err := buildRegisterURL(rawHost, "demo", shared.ProxyModeLegacyFramed, shared.TargetKindUnknown)
	if err != nil {
		t.Fatalf("buildRegisterURL(%q) error: %v", rawHost, err)
	}
	return u.String()
}

func TestResolveRegisterDialAddr(t *testing.T) {
	uWSS, err := buildRegisterURL("register-https-proxy.example.com", "demo", shared.ProxyModeLegacyFramed, shared.TargetKindUnknown)
	if err != nil {
		t.Fatalf("buildRegisterURL(wss) error: %v", err)
	}
	uWS, err := buildRegisterURL("http://register-https-proxy.example.com", "demo", shared.ProxyModeLegacyFramed, shared.TargetKindUnknown)
	if err != nil {
		t.Fatalf("buildRegisterURL(ws) error: %v", err)
	}

	cases := []struct {
		name        string
		raw         string
		urlMode     string
		want        string
		wantErrPart string
	}{
		{name: "empty", raw: "", urlMode: "wss", want: ""},
		{name: "ipv4 with default 443", raw: "10.1.2.3", urlMode: "wss", want: "10.1.2.3:443"},
		{name: "ipv4 with explicit port", raw: "10.1.2.3:8443", urlMode: "wss", want: "10.1.2.3:8443"},
		{name: "ipv6 with default 443", raw: "fd00::1", urlMode: "wss", want: "[fd00::1]:443"},
		{name: "ipv6 with explicit port", raw: "[fd00::1]:8443", urlMode: "wss", want: "[fd00::1]:8443"},
		{name: "default 80 in ws mode", raw: "10.1.2.3", urlMode: "ws", want: "10.1.2.3:80"},
		{name: "reject malformed hostport", raw: "register.lan:8443:9000", urlMode: "wss", wantErrPart: "expected ip, ip:port or hostname[:port]"},
		{name: "reject invalid port", raw: "10.1.2.3:70000", urlMode: "wss", wantErrPart: "invalid --register-ip port"},
		{name: "reject scheme", raw: "http://10.1.2.3", urlMode: "wss", wantErrPart: "scheme is not allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targetURL := uWSS
			if tc.urlMode == "ws" {
				targetURL = uWS
			}
			got, err := resolveRegisterDialAddrWithLookup(tc.raw, targetURL, func(context.Context, string) ([]net.IPAddr, error) {
				return nil, errors.New("unexpected DNS lookup in this test")
			})
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestResolveRegisterDialAddrHostnameLookup(t *testing.T) {
	uWSS, err := buildRegisterURL("register-https-proxy.example.com", "demo", shared.ProxyModeLegacyFramed, shared.TargetKindUnknown)
	if err != nil {
		t.Fatalf("buildRegisterURL(wss) error: %v", err)
	}

	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "register.lan":
			return []net.IPAddr{
				{IP: net.ParseIP("fd00::10")},
				{IP: net.ParseIP("10.8.9.10")},
			}, nil
		case "ipv6-only.lan":
			return []net.IPAddr{
				{IP: net.ParseIP("fd00::99")},
			}, nil
		case "missing.lan":
			return nil, errors.New("no such host")
		default:
			return nil, errors.New("unexpected host")
		}
	}

	cases := []struct {
		name        string
		raw         string
		want        string
		wantErrPart string
	}{
		{name: "hostname default port", raw: "register.lan", want: "10.8.9.10:443"},
		{name: "hostname explicit port", raw: "register.lan:8443", want: "10.8.9.10:8443"},
		{name: "hostname ipv6 fallback", raw: "ipv6-only.lan", want: "[fd00::99]:443"},
		{name: "hostname invalid port", raw: "register.lan:70000", wantErrPart: "invalid --register-ip port"},
		{name: "hostname lookup error", raw: "missing.lan", wantErrPart: "resolve --register-ip host \"missing.lan\""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRegisterDialAddrWithLookup(tc.raw, uWSS, lookup)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestBuildRegisterURLExamples(t *testing.T) {
	got := mustBuildRegisterURL(t, "register-https-proxy.example.com")
	want := "wss://register-https-proxy.example.com/register?proxy_mode=legacy_framed&uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}

	got = mustBuildRegisterURL(t, "http://127.0.0.1:18080")
	want = "ws://127.0.0.1:18080/register?proxy_mode=legacy_framed&uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestParseProxyMode(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string
		wantErrPart string
	}{
		{name: "empty fallback", raw: "", want: shared.ProxyModeLegacyFramed},
		{name: "legacy alias", raw: "legacy", want: shared.ProxyModeLegacyFramed},
		{name: "transparent alias", raw: "transparent", want: shared.ProxyModeTransparentTunnel},
		{name: "canonical transparent", raw: shared.ProxyModeTransparentTunnel, want: shared.ProxyModeTransparentTunnel},
		{name: "invalid", raw: "bad-mode", wantErrPart: "invalid --proxy-mode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProxyMode(tc.raw)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestBuildRegisterURLProxyModeTransparent(t *testing.T) {
	gotURL, err := buildRegisterURL(
		"register-https-proxy.example.com",
		"demo",
		shared.ProxyModeTransparentTunnel,
		shared.TargetKindUnknown,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := gotURL.String()
	want := "wss://register-https-proxy.example.com/register?proxy_mode=transparent_tunnel&uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestValidateProxyModeTarget(t *testing.T) {
	tcpTarget, err := url.Parse("tcp://127.0.0.1:22")
	if err != nil {
		t.Fatalf("parse tcp target failed: %v", err)
	}
	httpTarget, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("parse http target failed: %v", err)
	}

	cases := []struct {
		name        string
		mode        string
		target      *url.URL
		wantErrPart string
	}{
		{name: "legacy allows http", mode: shared.ProxyModeLegacyFramed, target: httpTarget},
		{name: "transparent allows tcp", mode: shared.ProxyModeTransparentTunnel, target: tcpTarget},
		{
			name: "transparent allows ws",
			mode: shared.ProxyModeTransparentTunnel,
			target: func() *url.URL {
				u, _ := url.Parse("ws://127.0.0.1:8080/bridge")
				return u
			}(),
		},
		{name: "transparent rejects http", mode: shared.ProxyModeTransparentTunnel, target: httpTarget, wantErrPart: "supports only tcp:// or ws(s)://"},
		{name: "transparent rejects nil", mode: shared.ProxyModeTransparentTunnel, target: nil, wantErrPart: "requires -R tcp://host:port or ws(s)://"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProxyModeTarget(tc.mode, tc.target)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErrPart, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBuildRegisterURLWithTargetKind(t *testing.T) {
	gotURL, err := buildRegisterURL(
		"register-https-proxy.example.com",
		"demo",
		shared.ProxyModeTransparentTunnel,
		shared.TargetKindWebSocket,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := gotURL.String()
	want := "wss://register-https-proxy.example.com/register?proxy_mode=transparent_tunnel&target_kind=ws&uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestTargetKindFromTargetURL(t *testing.T) {
	cases := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "nil target", rawURL: "", want: shared.TargetKindUnknown},
		{name: "http", rawURL: "http://127.0.0.1:8080", want: shared.TargetKindHTTP},
		{name: "https", rawURL: "https://demo.example.com", want: shared.TargetKindHTTP},
		{name: "tcp", rawURL: "tcp://127.0.0.1:22", want: shared.TargetKindTCP},
		{name: "ws", rawURL: "ws://127.0.0.1:8080/ws", want: shared.TargetKindWebSocket},
		{name: "wss", rawURL: "wss://demo.example.com/ws", want: shared.TargetKindWebSocket},
		{name: "unknown", rawURL: "udp://127.0.0.1:53", want: shared.TargetKindUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var target *url.URL
			if tc.rawURL != "" {
				var err error
				target, err = url.Parse(tc.rawURL)
				if err != nil {
					t.Fatalf("parse rawURL failed: %v", err)
				}
			}
			got := targetKindFromTargetURL(target)
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
