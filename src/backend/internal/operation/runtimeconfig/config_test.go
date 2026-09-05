package runtimeconfig

import (
	"strings"
	"testing"
	"time"
)

func TestLoadUsesSafeDefaultsForExplicitStalwartMode(t *testing.T) {
	clearWorkerEnvironment(t)
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "stalwart")

	config, err := Load(30 * time.Second)
	if err != nil {
		t.Fatalf("加载默认 worker 配置失败：%v", err)
	}
	if config.PollInterval != time.Second || config.LeaseDuration != 30*time.Second ||
		config.AttemptTimeout != 20*time.Second || config.LeaseSafetyMargin != 5*time.Second ||
		config.BaseRetryDelay != 5*time.Second || config.MaxRetryDelay != 5*time.Minute ||
		config.MaxAttempts != 8 {
		t.Fatalf("默认 worker 配置不符合契约：%+v", config)
	}
}

func TestLoadRejectsMissingOrUnknownMailCoreMode(t *testing.T) {
	clearWorkerEnvironment(t)
	if _, err := Load(30 * time.Second); err == nil {
		t.Fatal("缺少 mail-core 模式时必须拒绝启动")
	}
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "fake")
	if _, err := Load(30 * time.Second); err == nil {
		t.Fatal("生产 worker 不得装配 fake mail-core")
	}
}

func TestLoadRejectsUnsafeTimingWithoutEchoingValue(t *testing.T) {
	clearWorkerEnvironment(t)
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "stalwart")
	const secretLikeValue = "postgres://name:password@example.invalid"
	t.Setenv("MAIL_SUITE_WORKER_ATTEMPT_TIMEOUT", secretLikeValue)

	_, err := Load(30 * time.Second)
	if err == nil || strings.Contains(err.Error(), secretLikeValue) {
		t.Fatalf("期望脱敏的 worker 时长错误，实际为 %v", err)
	}
}

func TestLoadRequiresLeaseAndShutdownWindowsToCoverAttempt(t *testing.T) {
	clearWorkerEnvironment(t)
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "stalwart")
	if _, err := Load(24 * time.Second); err == nil {
		t.Fatal("关闭窗口小于 attempt 加回执余量时必须拒绝启动")
	}

	t.Setenv("MAIL_SUITE_WORKER_LEASE_DURATION", "10s")
	if _, err := Load(30 * time.Second); err == nil {
		t.Fatal("lease 小于 attempt 加回执余量时必须拒绝启动")
	}
}

func TestLoadAcceptsValidatedOverrides(t *testing.T) {
	clearWorkerEnvironment(t)
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "stalwart")
	t.Setenv("MAIL_SUITE_WORKER_POLL_INTERVAL", "2s")
	t.Setenv("MAIL_SUITE_WORKER_LEASE_DURATION", "40s")
	t.Setenv("MAIL_SUITE_WORKER_ATTEMPT_TIMEOUT", "25s")
	t.Setenv("MAIL_SUITE_WORKER_LEASE_SAFETY_MARGIN", "5s")
	t.Setenv("MAIL_SUITE_WORKER_BASE_RETRY_DELAY", "10s")
	t.Setenv("MAIL_SUITE_WORKER_MAX_RETRY_DELAY", "10m")
	t.Setenv("MAIL_SUITE_WORKER_MAX_ATTEMPTS", "12")

	config, err := Load(35 * time.Second)
	if err != nil {
		t.Fatalf("加载覆盖后的 worker 配置失败：%v", err)
	}
	if config.PollInterval != 2*time.Second || config.LeaseDuration != 40*time.Second ||
		config.AttemptTimeout != 25*time.Second || config.MaxAttempts != 12 {
		t.Fatalf("worker 覆盖配置不正确：%+v", config)
	}
}

// clearWorkerEnvironment 防止开发机环境变量影响配置测试。
func clearWorkerEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MAIL_SUITE_MAIL_CORE_MODE", "")
	for _, name := range durationEnvironmentNames {
		t.Setenv(name, "")
	}
	t.Setenv("MAIL_SUITE_WORKER_MAX_ATTEMPTS", "")
}
