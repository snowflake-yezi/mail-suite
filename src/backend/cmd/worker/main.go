// Package main 提供 operation worker 与 reconciler 进程入口。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, "worker", "127.0.0.1:8081"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "worker 启动或运行失败：", err)
		os.Exit(1)
	}
}
