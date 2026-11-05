package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clock-p/clockbridge/internal/gateway"
)

// Exercise the real entrypoint in a subprocess: both parsers may call os.Exit.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("CLOCKBRIDGE_TEST_CLI") != "1" {
		return
	}
	os.Args = append([]string{"clockbridge-cli"}, flag.Args()...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flag.CommandLine.Usage = func() { flag.Usage() }
	version = "bridge-test"
	main()
	os.Exit(0)
}

func invokeCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "CLOCKBRIDGE_TEST_CLI=1")
	out, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI timed out: %s", out)
	}
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func TestCommandLineDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
		code int
	}{
		{"default version", []string{"--version"}, "bridge-test\n", 0},
		{"default help", []string{"--help"}, "clockbridge-cli -R <target_url>", 0},
		{"selector is not a token value", []string{"--token=--frp", "--version"}, "bridge-test\n", 0},
		{"frpc version", []string{"--frp", "--version"}, "0.71.1\n", 0},
		{"frpc help", []string{"--frp", "--help"}, "frpc is the client of frp", 0},
		{"frpc tcpmux command", []string{"--frp", "tcpmux", "--help"}, "--mux", 0},
		{"frpc native identity flag", []string{"--frp", "http", "--help"}, "-i, --local-ip", 0},
		{"frpc errors never fall back to bridge", []string{"--frp", "-R", "http://127.0.0.1:8080"}, "unknown shorthand flag", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := invokeCLI(t, tc.args...)
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, output %q; want exit %d containing %q", code, out, tc.code, tc.want)
			}
		})
	}
}

func TestFRPNativeConfigVerification(t *testing.T) {
	file := filepath.Join(t.TempDir(), "frpc.toml")
	config := "serverAddr = \"127.0.0.1\"\nserverPort = 7000\n[[proxies]]\nname = \"tcp-service\"\ntype = \"tcpmux\"\nmultiplexer = \"httpconnect\"\ncustomDomains = [\"service.example.com\"]\nlocalPort = 2222\n"
	if err := os.WriteFile(file, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if out, code := invokeCLI(t, "--frp", "verify", "-c", file); code != 0 || !strings.Contains(out, "syntax is ok") {
		t.Fatalf("valid config: exit %d: %s", code, out)
	}
	if err := os.WriteFile(file, []byte("unknownOption = true\n"+config), 0600); err != nil {
		t.Fatal(err)
	}
	if out, code := invokeCLI(t, "--frp", "verify", "-c", file); code != 1 || !strings.Contains(out, "unknown field") {
		t.Fatalf("invalid config: exit %d: %s", code, out)
	}
}

func TestDefaultCLIStillForwardsHTTP(t *testing.T) {
	t.Setenv("HTTPS_PROXY_AGENT_TOKENS", "dispatch-test-token")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "bridge:"+r.URL.RequestURI())
	}))
	defer backend.Close()
	server := httptest.NewServer(gateway.NewServerFromEnv())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIProcess$", "--",
		"-x-token", "dispatch-test-token", "-R", backend.URL, "dispatch-test@"+server.URL)
	command.Env = append(os.Environ(), "CLOCKBRIDGE_TEST_CLI=1", "CLOCKBRIDGE_HTTPS_PROXY_TOKEN=", "CLOCKBRIDGE_HTTPS_PROXY_TOKEN_PATH=")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); command.Wait() }()
	client := &http.Client{Timeout: time.Second}
	for ctx.Err() == nil {
		response, err := client.Get(server.URL + "/dispatch-test/hello?q=1")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil {
				if string(body) != "bridge:/hello?q=1" {
					t.Fatalf("unexpected forwarded response: %q", body)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("default bridge CLI did not register and forward HTTP")
}
