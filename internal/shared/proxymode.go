package shared

import (
	"fmt"
	"strings"
)

const (
	ProxyModeLegacyFramed      = "legacy_framed"
	ProxyModeTransparentTunnel = "transparent_tunnel"

	HeaderProxyMode       = "X-Clockbridge-Proxy-Mode"
	HeaderProxyModeReason = "X-Clockbridge-Proxy-Mode-Reason"

	TargetKindUnknown   = "unknown"
	TargetKindHTTP      = "http"
	TargetKindTCP       = "tcp"
	TargetKindWebSocket = "ws"
)

// NormalizeProxyMode converts aliases into canonical values.
// Empty input falls back to legacy mode.
func NormalizeProxyMode(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", "legacy", ProxyModeLegacyFramed:
		return ProxyModeLegacyFramed, nil
	case "transparent", ProxyModeTransparentTunnel:
		return ProxyModeTransparentTunnel, nil
	default:
		return "", fmt.Errorf(
			"unsupported proxy mode %q, expected one of: %s, %s",
			raw,
			ProxyModeLegacyFramed,
			ProxyModeTransparentTunnel,
		)
	}
}

// NormalizeTargetKind normalizes register target_kind values.
// Empty input maps to unknown for backward compatibility with old agents.
func NormalizeTargetKind(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", TargetKindUnknown:
		return TargetKindUnknown, nil
	case TargetKindHTTP, "https":
		return TargetKindHTTP, nil
	case TargetKindTCP:
		return TargetKindTCP, nil
	case TargetKindWebSocket, "wss":
		return TargetKindWebSocket, nil
	default:
		return "", fmt.Errorf(
			"unsupported target kind %q, expected one of: %s, %s, %s",
			raw,
			TargetKindHTTP,
			TargetKindTCP,
			TargetKindWebSocket,
		)
	}
}
