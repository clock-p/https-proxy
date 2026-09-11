package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRunRetriesIdentityConflictUntilReleased(t *testing.T) {
	var attempts atomic.Int32
	connected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if attempts.Add(1) == 1 {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "uuid already active"), time.Now().Add(time.Second))
			return
		}
		close(connected)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	registerURL, _ := url.Parse("ws" + server.URL[4:] + "/register?uuid=reconnect-test")
	targetURL, _ := url.Parse("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(registerURL, "", "", "", targetURL).Run(ctx) }()
	select {
	case <-connected:
	case err := <-done:
		t.Fatalf("身份暂时占用不应结束重连：%v", err)
	case <-ctx.Done():
		t.Fatal("旧身份释放后未恢复连接")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消后转发未退出")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("连接次数 = %d，期望 2", got)
	}
}

func TestRunReportsBackoffAcrossShortConnections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	registerURL, _ := url.Parse("ws" + server.URL[4:] + "/register?uuid=flap-test")
	targetURL, _ := url.Parse("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a := New(registerURL, "", "", "", targetURL)
	var retries []ConnectionStatus
	a.OnStatus = func(status ConnectionStatus) {
		if status.State == "backoff" {
			retries = append(retries, status)
			if len(retries) == 2 {
				cancel()
			}
		}
	}
	_ = a.Run(ctx)
	if len(retries) != 2 {
		t.Fatalf("未观察到两次断线：%+v", retries)
	}
	second := retries[1]
	if second.Attempt != 2 || second.NextRetryAt == nil || second.LastError == "" || second.LastConnectedAt == nil {
		t.Fatalf("短暂连接清掉了累计退避或状态不完整：%+v", second)
	}
	if wait := second.NextRetryAt.Sub(*second.LastConnectedAt); wait < 1600*time.Millisecond {
		t.Fatalf("第二次等待应至少 1.6 秒，实际 %s", wait)
	}
}

func TestRunCancellationDuringIdentityConflict(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		attempts.Add(1)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4009, "uuid already active"), time.Now().Add(time.Second))
	}))
	defer server.Close()
	registerURL, _ := url.Parse("ws" + server.URL[4:])
	targetURL, _ := url.Parse("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := New(registerURL, "", "", "", targetURL)
	var states []string
	a.OnStatus = func(status ConnectionStatus) {
		states = append(states, status.State)
		if status.State == "identity_conflict" {
			if status.NextRetryAt == nil {
				t.Error("身份占用时缺少重试计划")
			}
			cancel()
		}
	}
	_ = a.Run(ctx)
	if len(states) != 4 || states[2] != "identity_conflict" || states[3] != "stopped" || attempts.Load() != 1 {
		t.Fatalf("状态或停止行为错误：states=%v attempts=%d", states, attempts.Load())
	}
}
