package main

import (
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
		{name: "reject host name", raw: "register.lan", urlMode: "wss", wantErrPart: "expected ip or ip:port"},
		{name: "reject invalid port", raw: "10.1.2.3:70000", urlMode: "wss", wantErrPart: "invalid --register-ip port"},
		{name: "reject scheme", raw: "http://10.1.2.3", urlMode: "wss", wantErrPart: "scheme is not allowed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targetURL := uWSS
			if tc.urlMode == "ws" {
				targetURL = uWS
			}
			got, err := resolveRegisterDialAddr(tc.raw, targetURL)
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
