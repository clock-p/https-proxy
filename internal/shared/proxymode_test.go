package shared

import "testing"

func TestNormalizeProxyMode(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string
		wantErrPart string
	}{
		{name: "empty fallback legacy", raw: "", want: ProxyModeLegacyFramed},
		{name: "legacy alias", raw: "legacy", want: ProxyModeLegacyFramed},
		{name: "legacy canonical", raw: ProxyModeLegacyFramed, want: ProxyModeLegacyFramed},
		{name: "transparent alias", raw: "transparent", want: ProxyModeTransparentTunnel},
		{name: "transparent canonical", raw: ProxyModeTransparentTunnel, want: ProxyModeTransparentTunnel},
		{name: "reject invalid", raw: "foo", wantErrPart: "unsupported proxy mode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeProxyMode(tc.raw)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
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

func TestNormalizeTargetKind(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string
		wantErrPart string
	}{
		{name: "empty fallback unknown", raw: "", want: TargetKindUnknown},
		{name: "unknown canonical", raw: TargetKindUnknown, want: TargetKindUnknown},
		{name: "http", raw: "http", want: TargetKindHTTP},
		{name: "https alias", raw: "https", want: TargetKindHTTP},
		{name: "tcp", raw: "tcp", want: TargetKindTCP},
		{name: "ws", raw: "ws", want: TargetKindWebSocket},
		{name: "wss alias", raw: "wss", want: TargetKindWebSocket},
		{name: "reject invalid", raw: "foo", wantErrPart: "unsupported target kind"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTargetKind(tc.raw)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrPart)
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
