package service

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	platformconfig "github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/config"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/lifecycle"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/probe"
)

// backgroundRunnerFunc 让服务集成测试观测后台 runner 是否真正进入执行。
type backgroundRunnerFunc func(context.Context) error

// Run 执行测试提供的后台任务行为。
func (run backgroundRunnerFunc) Run(ctx context.Context) error {
	return run(ctx)
}

// checkerFunc 为服务集成测试提供不会访问外部依赖的附加就绪条件。
type checkerFunc func(context.Context) error

// Check 执行测试提供的就绪检查行为。
func (check checkerFunc) Check(ctx context.Context) error {
	return check(ctx)
}

func TestRunWithBackgroundStartsBuiltRunner(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite:test@127.0.0.1:1/mail_suite?sslmode=disable")
	t.Setenv("MAIL_SUITE_HTTP_ADDRESS", "127.0.0.1:0")
	t.Setenv("MAIL_SUITE_SHUTDOWN_TIMEOUT", "1s")

	ctx, cancel := context.WithCancel(context.Background())
	var cancelOnce sync.Once
	stop := func() { cancelOnce.Do(cancel) }
	t.Cleanup(stop)

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunWithBackground(
			ctx,
			"background-test",
			"127.0.0.1:0",
			func(
				context.Context,
				*pgxpool.Pool,
				*slog.Logger,
				platformconfig.Config,
			) (lifecycle.Runner, probe.Checker, error) {
				return backgroundRunnerFunc(func(runContext context.Context) error {
					close(entered)
					<-runContext.Done()
					return nil
				}), checkerFunc(func(context.Context) error { return nil }), nil
			},
		)
	}()

	select {
	case <-entered:
		stop()
	case <-time.After(time.Second):
		stop()
		<-done
		t.Fatal("构造器返回的后台 runner 未进入执行")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("后台 runner 随服务停止失败：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务未在取消后有界停止")
	}
}
