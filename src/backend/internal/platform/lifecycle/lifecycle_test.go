package lifecycle

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeStopsAcceptingRequestsAfterCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建测试监听器失败：%v", err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	shutdownStarted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, server, func() { close(shutdownStarted) }, time.Second)
	}()

	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatalf("服务启动后请求失败：%v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		cancel()
		t.Fatalf("期望 204，实际为 %d", response.StatusCode)
	}

	cancel()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("取消上下文后没有进入关闭阶段")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅关闭失败：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务没有在截止时间内退出")
	}

	client := &http.Client{Timeout: 200 * time.Millisecond}
	if response, err := client.Get("http://" + listener.Addr().String()); err == nil {
		_ = response.Body.Close()
		t.Fatal("服务关闭后不应继续接受请求")
	}
}
