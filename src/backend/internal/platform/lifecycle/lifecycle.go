// Package lifecycle 管理 HTTP 服务的监听与有界优雅关闭。
package lifecycle

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Serve 在 listener 上运行服务，并在上下文取消后先撤销就绪再有界关闭。
func Serve(
	ctx context.Context,
	listener net.Listener,
	server *http.Server,
	beginShutdown func(),
	shutdownTimeout time.Duration,
) error {
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		beginShutdown()
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return errors.New("HTTP 服务未能在截止时间内关闭")
		}

		err := <-serverErrors
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
