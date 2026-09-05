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

// BackgroundBuilder 在共享基础设施建立后装配后台任务和额外就绪条件。
type BackgroundBuilder func(
	context.Context,
	*pgxpool.Pool,
	*slog.Logger,
	config.Config,
) (lifecycle.Runner, probe.Checker, error)

// Run 启动指定服务的健康端点、可选业务路由和 PostgreSQL 就绪检查，直到上下文取消。
func Run(ctx context.Context, serviceName, defaultHTTPAddress string, builders ...RouteBuilder) error {
	return run(ctx, serviceName, defaultHTTPAddress, nil, builders...)
}

// RunWithBackground 启动健康服务与一个必须共同存活的后台任务。
func RunWithBackground(
	ctx context.Context,
	serviceName string,
	defaultHTTPAddress string,
	backgroundBuilder BackgroundBuilder,
	builders ...RouteBuilder,
) error {
	if backgroundBuilder == nil {
		return errors.New("后台任务构造器不能为空")
	}
	return run(ctx, serviceName, defaultHTTPAddress, backgroundBuilder, builders...)
}

// run 统一装配长运行进程的数据库、路由、后台任务、探针和关闭顺序。
func run(
	ctx context.Context,
	serviceName string,
	defaultHTTPAddress string,
	backgroundBuilder BackgroundBuilder,
	builders ...RouteBuilder,
) error {
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
	readinessCheckers := []probe.Checker{database.NewChecker(pool)}
	var background lifecycle.Runner
	var additionalChecker probe.Checker
	if backgroundBuilder != nil {
		background, additionalChecker, err = backgroundBuilder(ctx, pool, logger, serviceConfig)
		if err != nil {
			return err
		}
		if background == nil || additionalChecker == nil {
			return errors.New("后台任务装配结果无效")
		}
		readinessCheckers = append(readinessCheckers, additionalChecker)
	}
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

	probeState := probe.NewState(probe.NewCheckerGroup(readinessCheckers...), serviceConfig.ProbeTimeout)
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

	runners := make([]lifecycle.Runner, 0, 1)
	if background != nil {
		runners = append(runners, background)
	}
	err = lifecycle.Serve(
		ctx,
		listener,
		server,
		probeState.BeginShutdown,
		serviceConfig.ShutdownTimeout,
		runners...,
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
