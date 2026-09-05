// Package main 提供 operation worker 与 reconciler 进程入口。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore/stalwart"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/operation"
	operationconfig "github.com/snowflake-yezi/mail-suite/src/backend/internal/operation/runtimeconfig"
	platformconfig "github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/config"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/lifecycle"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/probe"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/service"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		if err := service.CheckConfiguredHealth("127.0.0.1:8081", 2*time.Second); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.RunWithBackground(ctx, "worker", "127.0.0.1:8081", buildWorker); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "worker 启动或运行失败：", err)
		os.Exit(1)
	}
}

// buildWorker 装配真实 PostgreSQL ledger、Stalwart adapter 与持续扫描 runner。
func buildWorker(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	serviceConfig platformconfig.Config,
) (lifecycle.Runner, probe.Checker, error) {
	workerConfig, err := operationconfig.Load(serviceConfig.ShutdownTimeout)
	if err != nil {
		return nil, nil, err
	}
	mailCoreConfig, err := stalwart.LoadConfig()
	if err != nil {
		return nil, nil, err
	}
	adapter, err := stalwart.New(mailCoreConfig)
	if err != nil {
		return nil, nil, err
	}

	// repository 是 worker 对 operation ledger 的唯一持久化入口。
	repository := postgres.NewProvisioningRepository(pool)
	operationService, err := operation.NewService(repository, adapter, operation.Config{
		OwnerID:           uuid.New(),
		LeaseDuration:     workerConfig.LeaseDuration,
		AttemptTimeout:    workerConfig.AttemptTimeout,
		LeaseSafetyMargin: workerConfig.LeaseSafetyMargin,
		BaseRetryDelay:    workerConfig.BaseRetryDelay,
		MaxRetryDelay:     workerConfig.MaxRetryDelay,
		MaxAttempts:       workerConfig.MaxAttempts,
	})
	if err != nil {
		return nil, nil, err
	}
	runner, err := operation.NewRunner(operationService, workerConfig.PollInterval)
	if err != nil {
		return nil, nil, err
	}
	logger.InfoContext(
		ctx,
		"operation worker 已装配",
		"poll_interval", workerConfig.PollInterval,
		"lease_duration", workerConfig.LeaseDuration,
		"attempt_timeout", workerConfig.AttemptTimeout,
		"max_attempts", workerConfig.MaxAttempts,
	)
	return runner, adapter, nil
}
