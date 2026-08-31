// Package config 负责读取并校验进程启动配置。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	defaultProbeTimeout    = 2 * time.Second
)

// Config 描述长运行进程所需的基础设施配置，不包含业务配置。
type Config struct {
	// ServiceName 标识日志和遥测中的独立进程职责。
	ServiceName string
	// HTTPAddress 是内部健康服务监听地址。
	HTTPAddress string
	// DatabaseURL 是控制面 PostgreSQL 连接串，仅允许在进程内使用。
	DatabaseURL string
	// ShutdownTimeout 限制停止接收请求后的清理等待时间。
	ShutdownTimeout time.Duration
	// ProbeTimeout 限制单次外部依赖就绪检查的执行时间。
	ProbeTimeout time.Duration
	// OTLPEndpoint 是可选的 OTLP HTTP trace 接收地址。
	OTLPEndpoint string
}

// Load 从 MAIL_SUITE_ 前缀环境变量读取指定服务的配置。
func Load(serviceName, defaultHTTPAddress string) (Config, error) {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return Config{}, errors.New("服务名称不能为空")
	}

	databaseURL := strings.TrimSpace(os.Getenv("MAIL_SUITE_DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, errors.New("缺少必需配置 MAIL_SUITE_DATABASE_URL")
	}

	httpAddress := strings.TrimSpace(os.Getenv("MAIL_SUITE_HTTP_ADDRESS"))
	if httpAddress == "" {
		httpAddress = defaultHTTPAddress
	}
	if strings.TrimSpace(httpAddress) == "" {
		return Config{}, errors.New("健康服务监听地址不能为空")
	}

	shutdownTimeout, err := durationFromEnvironment("MAIL_SUITE_SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}
	probeTimeout, err := durationFromEnvironment("MAIL_SUITE_PROBE_TIMEOUT", defaultProbeTimeout)
	if err != nil {
		return Config{}, err
	}

	otlpEndpoint := strings.TrimSpace(os.Getenv("MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err := validateEndpoint(otlpEndpoint); err != nil {
		return Config{}, err
	}

	return Config{
		ServiceName:     serviceName,
		HTTPAddress:     httpAddress,
		DatabaseURL:     databaseURL,
		ShutdownTimeout: shutdownTimeout,
		ProbeTimeout:    probeTimeout,
		OTLPEndpoint:    otlpEndpoint,
	}, nil
}

// durationFromEnvironment 读取正数时长，错误信息只包含变量名而不回显原值。
func durationFromEnvironment(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("配置 %s 必须是正数时长", name)
	}
	return value, nil
}

// validateEndpoint 防止把不明确的 OTLP 地址传给 exporter。
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("配置 MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT 必须是 HTTP(S) URL")
	}
	if parsed.User != nil {
		return errors.New("配置 MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT 不得包含用户信息")
	}
	return nil
}
