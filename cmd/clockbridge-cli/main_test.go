package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func mustBuildRegisterURL(t *testing.T, rawHost string) string {
	t.Helper()
	u, err := buildRegisterURL(rawHost, "demo")
	if err != nil {
		t.Fatalf("buildRegisterURL(%q) error: %v", rawHost, err)
	}
	return u.String()
}

func TestResolveRegisterDialAddr(t *testing.T) {
	uWSS, err := buildRegisterURL("register-https-proxy.example.com", "demo")
	if err != nil {
		t.Fatalf("buildRegisterURL(wss) error: %v", err)
	}
	uWS, err := buildRegisterURL("http://register-https-proxy.example.com", "demo")
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
	uWSS, err := buildRegisterURL("register-https-proxy.example.com", "demo")
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
	want := "wss://register-https-proxy.example.com/register?uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}

	got = mustBuildRegisterURL(t, "http://127.0.0.1:18080")
	want = "ws://127.0.0.1:18080/register?uuid=demo"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
