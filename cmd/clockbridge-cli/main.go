package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/clock-p/clockbridge/internal/agent"
	"github.com/clock-p/clockbridge/internal/reverseproxy"
)

var version = "dev"

type mode int

const (
	modeUnknown mode = iota
	modeLocalForward
	modeRemoteForward
)

func main() {
	localSpec := flag.String("L", "", "local forward ([bind_addr:]port)，例如 127.0.0.1:28789")
	remoteTarget := flag.String("R", "", "remote forward target URL，例如 http://127.0.0.1:18789/")
	registerIP := flag.String("register-ip", "", "register connect override: ip/ip:port/hostname[:port]（仅覆盖 TCP 连接，Host/SNI 仍用 register host）")
	identityFile := flag.String("i", "", "identity file（Bearer token 文件，ssh 风格）")
	bearerTokenFlag := flag.String("token", "", "Bearer token（调试用，可选）")
	xToken := flag.String("x-token", "", "register 用 X-Token（兼容旧网关，可选）")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	runMode, err := resolveMode(*localSpec, *remoteTarget)
	if err != nil {
		log.Fatal(err)
	}

	bearerToken, bearerSource, err := resolveBearerToken(*bearerTokenFlag, *identityFile)
	if err != nil {
		log.Fatalf("resolve bearer token failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch runMode {
	case modeLocalForward:
		if strings.TrimSpace(*registerIP) != "" {
			log.Fatal("flag --register-ip is only valid in -R mode")
		}
		if err := runLocalForward(ctx, *localSpec, flag.Args(), bearerToken, bearerSource, *xToken); err != nil {
			log.Fatal(err)
		}
	case modeRemoteForward:
		if err := runRemoteForward(ctx, *remoteTarget, flag.Args(), *registerIP, bearerToken, bearerSource, *xToken); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("internal error: unexpected mode")
	}
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  clockbridge-cli -R <target_url> <uuid>@<register_host>")
	fmt.Fprintln(out, "  clockbridge-cli -L [bind_addr:]<port> <upstream_url>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  clockbridge-cli -i /path/to/token.txt -x-token <TOKEN> -R http://127.0.0.1:18789/ demo@register-https-proxy.example.com")
	fmt.Fprintln(out, "  clockbridge-cli -i /path/to/token.txt -L 127.0.0.1:28789 https://demo.example.com/")
	fmt.Fprintln(out, "")
	flag.PrintDefaults()
}

func resolveMode(localSpec, remoteTarget string) (mode, error) {
	hasLocal := strings.TrimSpace(localSpec) != ""
	hasRemote := strings.TrimSpace(remoteTarget) != ""
	if hasLocal && hasRemote {
		return modeUnknown, errors.New("flags -L and -R are mutually exclusive")
	}
	if !hasLocal && !hasRemote {
		return modeUnknown, errors.New("must provide either -L or -R")
	}
	if hasLocal {
		return modeLocalForward, nil
	}
	return modeRemoteForward, nil
}

func runLocalForward(
	ctx context.Context,
	localSpec string,
	args []string,
	bearerToken string,
	bearerSource string,
	xToken string,
) error {
	if len(args) != 1 {
		return errors.New("local forward (-L) expects exactly one destination argument: <upstream_url>")
	}
	listenAddr, err := parseListenAddr(localSpec)
	if err != nil {
		return err
	}
	upstream, err := parseHTTPURL(args[0], "upstream URL")
	if err != nil {
		return err
	}

	if strings.TrimSpace(xToken) != "" {
		log.Printf("[clockbridge-reverse] warning: -x-token is ignored in -L mode")
	}
	if bearerToken == "" {
		log.Printf("[clockbridge-reverse] warning: no Authorization bearer configured")
	} else {
		log.Printf("[clockbridge-reverse] upstream Authorization bearer: enabled source=%s", bearerSource)
	}

	rp := reverseproxy.New(listenAddr, upstream, bearerToken)
	if err := rp.Run(ctx); err != nil && err != context.Canceled {
		return fmt.Errorf("reverse proxy exit: %w", err)
	}
	return nil
}

func runRemoteForward(
	ctx context.Context,
	remoteTarget string,
	args []string,
	registerIP string,
	bearerToken string,
	bearerSource string,
	xToken string,
) error {
	if len(args) != 1 {
		return errors.New("remote forward (-R) expects exactly one destination argument: <uuid>@<register_host>")
	}
	target, err := parseHTTPURL(remoteTarget, "remote target URL")
	if err != nil {
		return err
	}
	uuid, registerHost, err := parseDestination(args[0])
	if err != nil {
		return err
	}
	registerURL, err := buildRegisterURL(registerHost, uuid)
	if err != nil {
		return err
	}
	registerDialAddr, err := resolveRegisterDialAddr(registerIP, registerURL)
	if err != nil {
		return err
	}

	if strings.TrimSpace(xToken) == "" && bearerToken == "" {
		log.Printf("[clockbridge-cli] warning: no auth headers configured (X-Token/Authorization)")
	} else {
		if strings.TrimSpace(xToken) != "" {
			log.Printf("[clockbridge-cli] register X-Token: enabled")
		}
		if bearerToken != "" {
			log.Printf("[clockbridge-cli] register Authorization bearer: enabled source=%s", bearerSource)
		}
	}
	if registerDialAddr != "" {
		log.Printf(
			"[clockbridge-cli] register dial override: connect=%s host_sni=%s",
			registerDialAddr,
			registerURL.Hostname(),
		)
	}

	a := agent.New(registerURL, registerDialAddr, strings.TrimSpace(xToken), bearerToken, target)
	if err := a.Run(ctx); err != nil && err != context.Canceled {
		return fmt.Errorf("agent exit: %w", err)
	}
	return nil
}

func parseListenAddr(raw string) (string, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return "", errors.New("invalid -L: empty listen spec")
	}
	if isDigits(spec) {
		return "127.0.0.1:" + spec, nil
	}
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		return "", fmt.Errorf("invalid -L listen spec %q, expected [bind_addr:]port", spec)
	}
	if !isDigits(port) {
		return "", fmt.Errorf("invalid -L listen port %q", port)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func parseDestination(raw string) (uuid string, registerHost string, err error) {
	spec := strings.TrimSpace(raw)
	parts := strings.SplitN(spec, "@", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid destination %q, expected <uuid>@<register_host>", spec)
	}
	uuid = strings.TrimSpace(parts[0])
	registerHost = strings.TrimSpace(parts[1])
	if uuid == "" || registerHost == "" {
		return "", "", fmt.Errorf("invalid destination %q, expected <uuid>@<register_host>", spec)
	}
	return uuid, registerHost, nil
}

func buildRegisterURL(registerHost, uuid string) (*url.URL, error) {
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
		// keep as-is
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
	u.RawQuery = q.Encode()
	return u, nil
}

func resolveRegisterDialAddr(raw string, registerURL *url.URL) (string, error) {
	return resolveRegisterDialAddrWithLookup(raw, registerURL, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return net.DefaultResolver.LookupIPAddr(ctx, host)
	})
}

func resolveRegisterDialAddrWithLookup(
	raw string,
	registerURL *url.URL,
	lookup func(context.Context, string) ([]net.IPAddr, error),
) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
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

func parseHTTPURL(raw string, field string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("%s is empty", field)
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("invalid %s %q: only http/https are supported", field, raw)
	}
	if strings.TrimSpace(u.Host) == "" {
		return nil, fmt.Errorf("invalid %s %q: missing host", field, raw)
	}
	return u, nil
}

func resolveBearerToken(tokenFlag, identityFile string) (token string, source string, err error) {
	tokenFromFlag := strings.TrimSpace(tokenFlag)
	if tokenFromFlag != "" {
		return tokenFromFlag, "flag:--token", nil
	}

	identityPath := strings.TrimSpace(identityFile)
	if identityPath != "" {
		value, readErr := readTokenFromFile(identityPath)
		if readErr != nil {
			return "", "", readErr
		}
		return value, "flag:-i", nil
	}

	tokenFromEnv := strings.TrimSpace(os.Getenv("CLOCKBRIDGE_HTTPS_PROXY_TOKEN"))
	if tokenFromEnv != "" {
		return tokenFromEnv, "env:CLOCKBRIDGE_HTTPS_PROXY_TOKEN", nil
	}

	tokenPathEnv := strings.TrimSpace(os.Getenv("CLOCKBRIDGE_HTTPS_PROXY_TOKEN_PATH"))
	if tokenPathEnv != "" {
		value, readErr := readTokenFromFile(tokenPathEnv)
		if readErr != nil {
			return "", "", readErr
		}
		return value, "env:CLOCKBRIDGE_HTTPS_PROXY_TOKEN_PATH", nil
	}

	return "", "none", nil
}

func readTokenFromFile(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", filePath, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("token file is empty: %s", filePath)
	}
	return value, nil
}

func isDigits(input string) bool {
	if input == "" {
		return false
	}
	for _, ch := range input {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
