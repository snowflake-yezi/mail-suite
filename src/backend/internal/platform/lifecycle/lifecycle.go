// Package lifecycle 管理 HTTP 服务的监听与有界优雅关闭。
package lifecycle

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Runner 表示必须与 HTTP 健康服务共同存活并响应取消的后台任务。
type Runner interface {
	// Run 持续执行任务，直到上下文取消或发生不可恢复错误。
	Run(context.Context) error
}

// Serve 在 listener 上运行服务，并在上下文取消后先撤销就绪再有界关闭。
func Serve(
	ctx context.Context,
	listener net.Listener,
	server *http.Server,
	beginShutdown func(),
	shutdownTimeout time.Duration,
	runners ...Runner,
) error {
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()
	runnerErrors := make(chan error, len(runners))
	for _, runner := range runners {
		go func(current Runner) {
			if current == nil {
				runnerErrors <- errors.New("后台任务无效")
				return
			}
			runnerErrors <- current.Run(runContext)
		}(runner)
	}

	serverFinished := false
	finishedRunners := 0
	var result error
	select {
	case err := <-serverErrors:
		serverFinished = true
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result = err
	case err := <-runnerErrors:
		finishedRunners++
		if err == nil && runContext.Err() == nil {
			err = errors.New("后台任务意外停止")
		}
		result = err
	case <-ctx.Done():
	}

	beginShutdown()
	cancelRun()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownContext); err != nil {
		_ = server.Close()
		if result == nil {
			result = errors.New("HTTP 服务未能在截止时间内关闭")
		}
	}
	if !serverFinished {
		select {
		case err := <-serverErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) && result == nil {
				result = err
			}
		case <-shutdownContext.Done():
			_ = server.Close()
			if result == nil {
				result = errors.New("HTTP 服务未能在截止时间内关闭")
			}
		}
	}
	for finishedRunners < len(runners) {
		select {
		case err := <-runnerErrors:
			finishedRunners++
			if err != nil && !errors.Is(err, context.Canceled) && result == nil {
				result = err
			}
		case <-shutdownContext.Done():
			if result == nil {
				result = errors.New("后台任务未能在截止时间内关闭")
			}
			return result
		}
	}
	return result
}
