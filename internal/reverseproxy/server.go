package reverseproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

type Server struct {
	listenAddr  string
	upstreamURL *url.URL
	bearerToken string
	transport   *http.Transport
}

func New(listenAddr string, upstreamURL *url.URL, bearerToken string) *Server {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Server{
		listenAddr:  listenAddr,
		upstreamURL: upstreamURL,
		bearerToken: bearerToken,
		transport:   transport,
	}
}

func (s *Server) Run(ctx context.Context) error {
	proxy := httputil.NewSingleHostReverseProxy(s.upstreamURL)
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		// Keep Host aligned with upstream virtual host routing.
		req.Host = s.upstreamURL.Host
		if s.bearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+s.bearerToken)
		}
	}
	proxy.Transport = s.transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[https-proxy-reverse] upstream error method=%s path=%s err=%v", req.Method, req.URL.String(), err)
		http.Error(rw, "bad gateway", http.StatusBadGateway)
	}

	server := &http.Server{
		Addr:              s.listenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)

	go func() {
		log.Printf("[https-proxy-reverse] listening=%s upstream=%s", s.listenAddr, s.upstreamURL.String())
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listen failed: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		err := <-errCh
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("shutdown failed: %w", err)
		}
		return ctx.Err()
	}
}
