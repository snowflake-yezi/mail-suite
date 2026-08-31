package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsMissingDatabaseURL(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")

	_, err := Load("api", "127.0.0.1:8080")
	if err == nil || !strings.Contains(err.Error(), "MAIL_SUITE_DATABASE_URL") {
		t.Fatalf("期望缺少数据库配置错误，实际为 %v", err)
	}
}

func TestLoadRejectsInvalidDurationWithoutEchoingValue(t *testing.T) {
	const secretLikeValue = "postgres://name:password@example.invalid"
	t.Setenv("MAIL_SUITE_DATABASE_URL", secretLikeValue)
	t.Setenv("MAIL_SUITE_PROBE_TIMEOUT", secretLikeValue)

	_, err := Load("api", "127.0.0.1:8080")
	if err == nil {
		t.Fatal("期望无效时长被拒绝")
	}
	if strings.Contains(err.Error(), secretLikeValue) {
		t.Fatal("配置错误不得回显原值")
	}
}

func TestLoadUsesValidatedOverrides(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	t.Setenv("MAIL_SUITE_HTTP_ADDRESS", "127.0.0.1:19090")
	t.Setenv("MAIL_SUITE_SHUTDOWN_TIMEOUT", "7s")
	t.Setenv("MAIL_SUITE_PROBE_TIMEOUT", "750ms")
	t.Setenv("MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	got, err := Load("worker", "127.0.0.1:8081")
	if err != nil {
		t.Fatalf("期望配置有效，实际错误为 %v", err)
	}
	if got.ServiceName != "worker" || got.HTTPAddress != "127.0.0.1:19090" {
		t.Fatalf("配置标识或监听地址不正确：%+v", got)
	}
	if got.ShutdownTimeout != 7*time.Second || got.ProbeTimeout != 750*time.Millisecond {
		t.Fatalf("超时配置不正确：%+v", got)
	}
}

func TestLoadRejectsOTLPEndpointWithCredentials(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	t.Setenv("MAIL_SUITE_OTEL_EXPORTER_OTLP_ENDPOINT", "https://name:password@example.invalid")

	_, err := Load("api", "127.0.0.1:8080")
	if err == nil || strings.Contains(err.Error(), "password") {
		t.Fatalf("期望脱敏的 OTLP 地址错误，实际为 %v", err)
	}
}
