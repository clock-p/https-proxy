package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/clock-p/https-proxy/internal/agent"
)

var version = "dev"

func main() {
	registerURLStr := flag.String("register-url", "", "注册地址：ws(s)://host/https-proxy/register?uuid=...")
	token := flag.String("token", "", "注册用 X-Token")
	targetStr := flag.String("target", "", "目标 base URL，例如 http://127.0.0.1:8080/aaa")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *registerURLStr == "" || *token == "" || *targetStr == "" {
		log.Fatalf("missing required flags: --register-url --token --target")
	}

	registerURL, err := url.Parse(*registerURLStr)
	if err != nil {
		log.Fatalf("bad --register-url: %v", err)
	}
	target, err := url.Parse(*targetStr)
	if err != nil {
		log.Fatalf("bad --target: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := agent.New(registerURL, *token, target)
	if err := a.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("agent exit: %v", err)
	}
}
