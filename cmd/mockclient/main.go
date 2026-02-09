package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:18080/u1", "Gateway base，例如 http://127.0.0.1:18080/u1")
	mode := flag.String("mode", "http", "模式：http|stream|ws|ws-close-client|ws-close-up|resp-trailer|req-trailer|interim|big-post|http-conc|ws-conc")
	size := flag.Int64("size", 50*1024*1024, "big-post 发送 body 大小（字节）")
	n := flag.Int("n", 10, "并发数（用于 http-conc / ws-conc）")
	flag.Parse()

	switch *mode {
	case "http":
		doHTTP(*base + "/hello")
	case "stream":
		doStream(*base + "/stream")
	case "ws":
		doWS(*base + "/ws")
	case "ws-close-client":
		doWSCloseClient(*base + "/ws")
	case "ws-close-up":
		doWSCloseUpstream(*base + "/ws-close")
	case "resp-trailer":
		doRespTrailer(*base + "/trailer")
	case "req-trailer":
		doReqTrailer(*base + "/req-trailer")
	case "interim":
		doInterim(*base + "/interim")
	case "big-post":
		doBigPost(*base+"/upload", *size)
	case "http-conc":
		doHTTPConcurrent(*base+"/hello", *n)
	case "ws-conc":
		doWSConcurrent(*base+"/ws", *n)
	default:
		log.Fatalf("unknown --mode: %s", *mode)
	}
}

func doHTTP(u string) {
	resp, err := http.Get(u)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d\n%s\n", resp.StatusCode, string(b))
}

func doStream(u string) {
	resp, err := http.Get(u)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	fmt.Printf("status=%d\n", resp.StatusCode)
	buf := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			fmt.Print(string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func doWS(httpBase string) {
	u, err := url.Parse(httpBase)
	if err != nil {
		log.Fatal(err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(u.String(), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = ws.Close() }()

	_ = ws.WriteMessage(websocket.TextMessage, []byte("hello over ws"))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("mt=%d msg=%s\n", mt, string(data))
}

func doWSCloseClient(httpBase string) {
	u := mustWSURL(httpBase)
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(u.String(), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	_ = ws.WriteMessage(websocket.TextMessage, []byte("closing from client"))
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, "client closing"), time.Now().Add(2*time.Second))
	_, _, err = ws.ReadMessage()
	if err != nil {
		fmt.Printf("client closed: %v\n", err)
		return
	}
}

func doWSCloseUpstream(httpBase string) {
	u := mustWSURL(httpBase)
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(u.String(), nil)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	_ = ws.WriteMessage(websocket.TextMessage, []byte("hello"))
	_, _, err = ws.ReadMessage()
	if err != nil {
		fmt.Printf("upstream closed: %v\n", err)
		return
	}
}

func mustWSURL(httpBase string) *url.URL {
	u, err := url.Parse(httpBase)
	if err != nil {
		log.Fatal(err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return u
}

func doRespTrailer(u string) {
	resp, err := http.Get(u)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	fmt.Printf("status=%d trailer=%s\n", resp.StatusCode, resp.Trailer.Get("X-Resp-Trailer"))
}

func doReqTrailer(u string) {
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	req, err := http.NewRequest("POST", u, pr)
	if err != nil {
		log.Fatal(err)
	}
	req.Trailer = http.Header{"X-Req-Trailer": nil}
	req.Header.Set("Trailer", "X-Req-Trailer")

	go func() {
		_, _ = pw.Write([]byte("hello"))
		req.Trailer.Set("X-Req-Trailer", "req-ok")
		_ = pw.Close()
	}()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, string(b))
}

func doInterim(u string) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%s header.X-Interim=%s header.X-Final=%s\n", resp.StatusCode, string(b), resp.Header.Get("X-Interim"), resp.Header.Get("X-Final"))
}

type repeatReader struct{}

func (repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func doBigPost(u string, size int64) {
	body := io.LimitReader(repeatReader{}, size)
	req, err := http.NewRequest("POST", u, io.NopCloser(body))
	if err != nil {
		log.Fatal(err)
	}
	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, string(b))
}

func doHTTPConcurrent(u string, n int) {
	var ok int64
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(u)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("http-conc ok=%d/%d dur=%s\n", ok, n, time.Since(start))
}

func doWSConcurrent(httpBase string, n int) {
	u := mustWSURL(httpBase)
	var ok int64
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
			ws, _, err := d.Dial(u.String(), nil)
			if err != nil {
				return
			}
			defer func() { _ = ws.Close() }()
			_ = ws.WriteMessage(websocket.TextMessage, []byte("hello"))
			_, _, err = ws.ReadMessage()
			if err != nil {
				return
			}
			atomic.AddInt64(&ok, 1)
		}()
	}
	wg.Wait()
	fmt.Printf("ws-conc ok=%d/%d dur=%s\n", ok, n, time.Since(start))
}
