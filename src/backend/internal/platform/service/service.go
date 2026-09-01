// Package service 组装长运行进程的基础设施并统一启动顺序。
package service

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/config"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/database"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/lifecycle"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/logging"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/probe"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/telemetry"
)

// RouteBuilder 在数据库连接建立后构造当前进程选择启用的业务路由。
type RouteBuilder func(context.Context, *pgxpool.Pool, *slog.Logger) (probe.RouteRegistrar, error)

// Run 启动指定服务的健康端点、可选业务路由和 PostgreSQL 就绪检查，直到上下文取消。
func Run(ctx context.Context, serviceName, defaultHTTPAddress string, builders ...RouteBuilder) error {
	serviceConfig, err := config.Load(serviceName, defaultHTTPAddress)
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, serviceName, buildinfo.Version, buildinfo.Commit)

	shutdownTelemetry, err := telemetry.New(
		ctx,
		serviceConfig.OTLPEndpoint,
		serviceName,
		buildinfo.Version,
	)
	if err != nil {
		return err
	}
	defer shutdownTelemetryWithLog(shutdownTelemetry, serviceConfig.ShutdownTimeout, logger)

	pool, err := database.Open(ctx, serviceConfig.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	registrars := make([]probe.RouteRegistrar, 0, len(builders))
	for _, builder := range builders {
		registrar, buildErr := builder(ctx, pool, logger)
		if buildErr != nil {
			return buildErr
		}
		if registrar != nil {
			registrars = append(registrars, registrar)
		}
	}

	probeState := probe.NewState(database.NewChecker(pool), serviceConfig.ProbeTimeout)
	server := &http.Server{
		Addr:              serviceConfig.HTTPAddress,
		Handler:           probe.NewHandler(probeState, logger, registrars...),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	listener, err := net.Listen("tcp", serviceConfig.HTTPAddress)
	if err != nil {
		return errors.New("健康服务监听失败")
	}
	logger.InfoContext(ctx, "服务开始监听", "address", listener.Addr().String())

	err = lifecycle.Serve(
		ctx,
		listener,
		server,
		probeState.BeginShutdown,
		serviceConfig.ShutdownTimeout,
	)
	if err == nil {
		logger.Info("服务已正常停止")
	}
	return err
}

// shutdownTelemetryWithLog 在独立截止时间内刷新遥测，失败时只写结构化错误。
func shutdownTelemetryWithLog(shutdown telemetry.Shutdown, timeout time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		logger.Error("遥测关闭失败")
	}
}
