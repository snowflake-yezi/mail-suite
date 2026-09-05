// Package main 提供无状态管理 API 进程入口。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/bootstrap"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/service"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		if err := service.CheckConfiguredHealth("127.0.0.1:8080", 2*time.Second); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, "api", "127.0.0.1:8080", bootstrap.Routes); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "api 启动或运行失败：", err)
		os.Exit(1)
	}
}
