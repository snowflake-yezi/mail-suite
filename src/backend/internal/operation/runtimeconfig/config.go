// Package runtimeconfig 负责读取并校验 operation worker 的业务运行参数。
package runtimeconfig

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	mailCoreModeStalwart    = "stalwart"
	defaultPollInterval     = time.Second
	defaultLeaseDuration    = 30 * time.Second
	defaultAttemptTimeout   = 20 * time.Second
	defaultLeaseSafety      = 5 * time.Second
	defaultBaseRetryDelay   = 5 * time.Second
	defaultMaximumRetry     = 5 * time.Minute
	defaultMaximumAttempts  = 8
	maximumConfiguredTiming = 24 * time.Hour
)

var durationEnvironmentNames = []string{
	"MAIL_SUITE_WORKER_POLL_INTERVAL",
	"MAIL_SUITE_WORKER_LEASE_DURATION",
	"MAIL_SUITE_WORKER_ATTEMPT_TIMEOUT",
	"MAIL_SUITE_WORKER_LEASE_SAFETY_MARGIN",
	"MAIL_SUITE_WORKER_BASE_RETRY_DELAY",
	"MAIL_SUITE_WORKER_MAX_RETRY_DELAY",
}

// Config 固定 worker 扫描、租约、执行和重试边界。
type Config struct {
	// PollInterval 是空队列后再次扫描 PostgreSQL ledger 的等待时间。
	PollInterval time.Duration
	// LeaseDuration 是单次 operation 领取的数据库租约时长。
	LeaseDuration time.Duration
	// AttemptTimeout 是 adapter ensure 与 inspect 共享的业务执行时长。
	AttemptTimeout time.Duration
	// LeaseSafetyMargin 是最终 fenced 数据库回执预留时间。
	LeaseSafetyMargin time.Duration
	// BaseRetryDelay 是第一次临时失败或 unknown 后的等待时间。
	BaseRetryDelay time.Duration
	// MaxRetryDelay 是指数退避允许达到的最大等待时间。
	MaxRetryDelay time.Duration
	// MaxAttempts 是 operation 转入 dead 前允许创建的 attempt 总数。
	MaxAttempts int
}

// Load 从 MAIL_SUITE_ 环境变量加载 worker 参数，并确保关闭窗口能够覆盖当前 attempt 收尾。
func Load(shutdownTimeout time.Duration) (Config, error) {
	if strings.TrimSpace(os.Getenv("MAIL_SUITE_MAIL_CORE_MODE")) != mailCoreModeStalwart {
		return Config{}, errors.New("配置 MAIL_SUITE_MAIL_CORE_MODE 必须显式设置为 stalwart")
	}

	values := make([]time.Duration, len(durationEnvironmentNames))
	defaults := []time.Duration{
		defaultPollInterval,
		defaultLeaseDuration,
		defaultAttemptTimeout,
		defaultLeaseSafety,
		defaultBaseRetryDelay,
		defaultMaximumRetry,
	}
	for index, name := range durationEnvironmentNames {
		value, err := durationFromEnvironment(name, defaults[index])
		if err != nil {
			return Config{}, err
		}
		values[index] = value
	}
	maximumAttempts, err := positiveIntegerFromEnvironment(
		"MAIL_SUITE_WORKER_MAX_ATTEMPTS",
		defaultMaximumAttempts,
	)
	if err != nil {
		return Config{}, err
	}

	config := Config{
		PollInterval:      values[0],
		LeaseDuration:     values[1],
		AttemptTimeout:    values[2],
		LeaseSafetyMargin: values[3],
		BaseRetryDelay:    values[4],
		MaxRetryDelay:     values[5],
		MaxAttempts:       maximumAttempts,
	}
	if config.LeaseDuration <= config.LeaseSafetyMargin ||
		config.AttemptTimeout > config.LeaseDuration-config.LeaseSafetyMargin {
		return Config{}, errors.New("worker attempt 与 lease 时间边界无效")
	}
	if shutdownTimeout <= config.LeaseSafetyMargin ||
		config.AttemptTimeout > shutdownTimeout-config.LeaseSafetyMargin {
		return Config{}, errors.New("MAIL_SUITE_SHUTDOWN_TIMEOUT 不足以完成 worker attempt 收尾")
	}
	if config.MaxRetryDelay < config.BaseRetryDelay {
		return Config{}, errors.New("worker 最大退避不得小于初始退避")
	}
	return config, nil
}

// durationFromEnvironment 读取有上限的正数时长，错误信息不回显环境变量原值。
func durationFromEnvironment(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 || value > maximumConfiguredTiming {
		return 0, fmt.Errorf("配置 %s 必须是 24 小时内的正数时长", name)
	}
	return value, nil
}

// positiveIntegerFromEnvironment 读取可安全传入 PostgreSQL int4 的正整数。
func positiveIntegerFromEnvironment(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value <= 0 || value > math.MaxInt32 {
		return 0, fmt.Errorf("配置 %s 必须是 int32 范围内的正整数", name)
	}
	return int(value), nil
}
