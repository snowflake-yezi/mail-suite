package lifecycle

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// runnerFunc 让生命周期测试精确控制后台任务的退出方式。
type runnerFunc func(context.Context) error

// Run 执行测试提供的后台任务行为。
func (run runnerFunc) Run(ctx context.Context) error {
	return run(ctx)
}

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

func TestServeCancelsBackgroundRunnerDuringShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建测试监听器失败：%v", err)
	}
	server := &http.Server{Handler: http.NotFoundHandler()}
	entered := make(chan struct{})
	stopped := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, server, func() {}, time.Second, runner)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("后台任务未启动")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("关闭时未取消后台任务")
	}
	if err = <-done; err != nil {
		t.Fatalf("包含后台任务的服务关闭失败：%v", err)
	}
}

func TestServeStopsHTTPWhenBackgroundRunnerFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建测试监听器失败：%v", err)
	}
	address := listener.Addr().String()
	server := &http.Server{Handler: http.NotFoundHandler()}
	expected := errors.New("worker failed")
	shutdownStarted := make(chan struct{})
	err = Serve(
		context.Background(),
		listener,
		server,
		func() { close(shutdownStarted) },
		time.Second,
		runnerFunc(func(context.Context) error { return expected }),
	)
	if !errors.Is(err, expected) {
		t.Fatalf("后台任务错误没有成为进程错误：%v", err)
	}
	select {
	case <-shutdownStarted:
	default:
		t.Fatal("后台任务失败时未撤销就绪")
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	if response, requestErr := client.Get("http://" + address); requestErr == nil {
		_ = response.Body.Close()
		t.Fatal("后台任务失败后 HTTP 监听仍可访问")
	}
}
