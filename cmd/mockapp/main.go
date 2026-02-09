package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "listen addr")
	base := flag.String("base", "/aaa", "base path prefix")
	flag.Parse()

	b := strings.TrimRight(*base, "/")
	if b == "" {
		b = ""
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc(b+"/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	})
	mux.HandleFunc(b+"/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			_, _ = fmt.Fprintf(w, "chunk %02d at %s\n", i, time.Now().Format(time.RFC3339Nano))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	mux.HandleFunc(b+"/trailer", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Trailer", "X-Resp-Trailer")
		_, _ = w.Write([]byte("body\n"))
		w.Header().Set("X-Resp-Trailer", "resp-ok")
	})
	mux.HandleFunc(b+"/req-trailer", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		v := r.Trailer.Get("X-Req-Trailer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(v))
	})
	mux.HandleFunc(b+"/upload", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"size":%d}`, n)))
	})
	mux.HandleFunc(b+"/interim", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Interim", "1")
		w.WriteHeader(100)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Final", "1")
		_, _ = w.Write([]byte("final\n"))
	})
	mux.HandleFunc(b+"/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			_ = ws.WriteMessage(mt, data)
		}
	})
	mux.HandleFunc(b+"/ws-close", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_, _, _ = ws.ReadMessage()
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "server closing"), time.Now().Add(2*time.Second))
	})

	log.Printf("[mockapp] listen=%s base=%s", *listen, b)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
