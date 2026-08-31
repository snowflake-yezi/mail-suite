// Package logging 建立不含业务正文的结构化进程日志。
package logging

import (
	"io"
	"log/slog"
)

// New 创建带服务和构建身份字段的 JSON logger。
func New(output io.Writer, serviceName, version, commit string) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler).With(
		"service", serviceName,
		"version", version,
		"commit", commit,
	)
}
