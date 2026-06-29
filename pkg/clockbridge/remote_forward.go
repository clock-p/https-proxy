package clockbridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/clock-p/clockbridge/internal/agent"
	"github.com/clock-p/clockbridge/internal/shared"
)

const (
	ProxyModeLegacyFramed      = shared.ProxyModeLegacyFramed
	ProxyModeTransparentTunnel = shared.ProxyModeTransparentTunnel

	TargetKindUnknown   = shared.TargetKindUnknown
	TargetKindHTTP      = shared.TargetKindHTTP
	TargetKindTCP       = shared.TargetKindTCP
	TargetKindWebSocket = shared.TargetKindWebSocket
)

type RemoteForwardConfig struct {
	TargetURL           string
	UUID                string
	RegisterHost        string
	RegisterIP          string
	ProxyMode           string
	RegisterXToken      string
	RegisterBearerToken string
}

type RemoteForward struct {
	registerURL         *url.URL
	registerDialAddr    string
	registerXToken      string
	registerBearerToken string
	target              *url.URL
}

func NewRemoteForward(config RemoteForwardConfig) (*RemoteForward, error) {
	proxyMode, err := ParseProxyMode(config.ProxyMode)
	if err != nil {
		return nil, err
	}
	target, err := ParseRemoteTargetURL(config.TargetURL, "remote target URL")
	if err != nil {
		return nil, err
	}
	if err := ValidateProxyModeTarget(proxyMode, target); err != nil {
		return nil, err
	}
	registerURL, err := BuildRegisterURL(config.RegisterHost, config.UUID, proxyMode, TargetKindFromTargetURL(target))
	if err != nil {
		return nil, err
	}
	registerDialAddr, err := ResolveRegisterDialAddr(config.RegisterIP, registerURL)
	if err != nil {
		return nil, err
	}
	return &RemoteForward{
		registerURL:         registerURL,
		registerDialAddr:    registerDialAddr,
		registerXToken:      strings.TrimSpace(config.RegisterXToken),
		registerBearerToken: strings.TrimSpace(config.RegisterBearerToken),
		target:              target,
	}, nil
}

func (f *RemoteForward) RegisterURL() *url.URL {
	if f == nil || f.registerURL == nil {
		return nil
	}
	clone := *f.registerURL
	return &clone
}

func (f *RemoteForward) RegisterDialAddr() string {
	if f == nil {
		return ""
	}
	return f.registerDialAddr
}

func (f *RemoteForward) Run(ctx context.Context) error {
	if f == nil {
		return errors.New("clockbridge remote forward is nil")
	}
	a := agent.New(f.registerURL, f.registerDialAddr, f.registerXToken, f.registerBearerToken, f.target)
	if err := a.Run(ctx); err != nil && err != context.Canceled {
		return fmt.Errorf("agent exit: %w", err)
	}
	return nil
}

func BuildRegisterURL(registerHost, uuid string, proxyMode string, targetKind string) (*url.URL, error) {
	raw := strings.TrimSpace(registerHost)
	if raw == "" {
		return nil, errors.New("register host is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid register host %q: %w", registerHost, err)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return nil, fmt.Errorf("unsupported register scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Host) == "" {
		return nil, fmt.Errorf("invalid register host %q: missing host", registerHost)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/register"
	}
	q := u.Query()
	q.Set("uuid", uuid)
	if strings.TrimSpace(proxyMode) != "" {
		q.Set("proxy_mode", proxyMode)
	}
	normalizedTargetKind, err := shared.NormalizeTargetKind(targetKind)
	if err != nil {
		return nil, fmt.Errorf("invalid target kind %q: %w", targetKind, err)
	}
	if normalizedTargetKind != shared.TargetKindUnknown {
		q.Set("target_kind", normalizedTargetKind)
	}
	u.RawQuery = q.Encode()
	return u, nil
}

func TargetKindFromTargetURL(target *url.URL) string {
	if target == nil {
		return shared.TargetKindUnknown
	}
	switch strings.ToLower(strings.TrimSpace(target.Scheme)) {
	case "http", "https":
		return shared.TargetKindHTTP
	case "tcp":
		return shared.TargetKindTCP
	case "ws", "wss":
		return shared.TargetKindWebSocket
	default:
		return shared.TargetKindUnknown
	}
}

func ParseProxyMode(raw string) (string, error) {
	value, err := shared.NormalizeProxyMode(raw)
	if err != nil {
		return "", fmt.Errorf("invalid --proxy-mode: %w", err)
	}
	return value, nil
}

func ValidateProxyModeTarget(proxyMode string, target *url.URL) error {
	if proxyMode != shared.ProxyModeTransparentTunnel {
		return nil
	}
	if target == nil {
		return errors.New("--proxy-mode transparent_tunnel requires -R tcp://host:port or ws(s)://host[:port]/... target")
	}
	scheme := strings.ToLower(strings.TrimSpace(target.Scheme))
	if scheme != "tcp" && scheme != "ws" && scheme != "wss" {
		return errors.New("--proxy-mode transparent_tunnel currently supports only tcp:// or ws(s):// targets")
	}
	return nil
}

func ResolveRegisterDialAddr(raw string, registerURL *url.URL) (string, error) {
	return ResolveRegisterDialAddrWithLookup(raw, registerURL, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	})
}

func ResolveRegisterDialAddrWithLookup(
	raw string,
	registerURL *url.URL,
	lookup func(context.Context, string) ([]net.IPAddr, error),
) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if registerURL == nil {
		return "", errors.New("register URL is nil")
	}
	if strings.Contains(value, "://") {
		return "", fmt.Errorf("invalid --register-ip %q: scheme is not allowed", raw)
	}

	defaultPort := registerURL.Port()
	if defaultPort == "" {
		switch strings.ToLower(strings.TrimSpace(registerURL.Scheme)) {
		case "wss", "https":
			defaultPort = "443"
		case "ws", "http":
			defaultPort = "80"
		default:
			return "", fmt.Errorf("invalid register URL scheme %q for --register-ip", registerURL.Scheme)
		}
	}

	if ip, err := netip.ParseAddr(value); err == nil {
		return net.JoinHostPort(ip.String(), defaultPort), nil
	}

	host := value
	port := defaultPort
	if splitHost, splitPort, err := net.SplitHostPort(value); err == nil {
		host = strings.TrimSpace(splitHost)
		port = strings.TrimSpace(splitPort)
	} else if addrErr, ok := err.(*net.AddrError); !ok || !strings.Contains(strings.ToLower(addrErr.Err), "missing port") {
		return "", fmt.Errorf("invalid --register-ip %q, expected ip, ip:port or hostname[:port]", raw)
	}

	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("invalid --register-ip %q, expected ip, ip:port or hostname[:port]", raw)
	}

	port = strings.TrimSpace(port)
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return "", fmt.Errorf("invalid --register-ip port %q", port)
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		return net.JoinHostPort(ip.String(), port), nil
	}

	if lookup == nil {
		return "", errors.New("internal error: register ip resolver is nil")
	}
	resolved, err := lookup(context.Background(), host)
	if err != nil {
		return "", fmt.Errorf("resolve --register-ip host %q: %w", host, err)
	}
	for _, item := range resolved {
		if ipv4 := item.IP.To4(); ipv4 != nil {
			return net.JoinHostPort(ipv4.String(), port), nil
		}
	}
	for _, item := range resolved {
		if ipv6 := item.IP.To16(); ipv6 != nil {
			return net.JoinHostPort(ipv6.String(), port), nil
		}
	}
	return "", fmt.Errorf("resolve --register-ip host %q: no A/AAAA record", host)
}

func ParseRemoteTargetURL(raw string, field string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("%s is empty", field)
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" && scheme != "tcp" && scheme != "ws" && scheme != "wss" {
		return nil, fmt.Errorf("invalid %s %q: only http/https/tcp/ws/wss are supported", field, raw)
	}
	if strings.TrimSpace(u.Host) == "" {
		return nil, fmt.Errorf("invalid %s %q: missing host", field, raw)
	}
	if scheme == "tcp" {
		if strings.TrimSpace(u.Port()) == "" {
			return nil, fmt.Errorf("invalid %s %q: tcp target requires explicit port", field, raw)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("invalid %s %q: tcp target must not include path", field, raw)
		}
	}
	return u, nil
}
