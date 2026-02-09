package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
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
	log.Printf("[https-proxy-gateway] listen=%s", *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Fatal(srv.ListenAndServe())
}
