// Package telemetry 配置可选的 OTLP trace 输出。
package telemetry

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	traceSDK "go.opentelemetry.io/otel/sdk/trace"
)

// Shutdown 释放遥测批处理资源；禁用遥测时仍可安全调用。
type Shutdown func(context.Context) error

// New 根据显式 endpoint 创建 trace provider，空 endpoint 表示保持无输出模式。
func New(ctx context.Context, endpoint, serviceName, version string) (Shutdown, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, errors.New("OTLP trace exporter 初始化失败")
	}

	serviceResource, err := resource.New(
		ctx,
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
			attribute.String("service.version", version),
		),
	)
	if err != nil {
		return nil, errors.New("遥测资源初始化失败")
	}

	provider := traceSDK.NewTracerProvider(
		traceSDK.WithBatcher(exporter),
		traceSDK.WithResource(serviceResource),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
