package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/clock-p/https-proxy/internal/gateway"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "listen address (http)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	s := gateway.NewServerFromEnv()
	maxHeaderBytes := gateway.MaxHeaderBytesFromEnv()
	readHeaderTimeout := durationFromEnv("HTTPS_PROXY_HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	idleTimeout := durationFromEnv("HTTPS_PROXY_HTTP_IDLE_TIMEOUT", 120*time.Second)
	log.Printf("[https-proxy-gateway] listen=%s", *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Fatal(srv.ListenAndServe())
}

func durationFromEnv(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return def
		}
		return d
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	return def
}
