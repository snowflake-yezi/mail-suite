package probe

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/logging"
)

// checkerFunc 让测试精确控制单次依赖检查结果。
type checkerFunc func(context.Context) error

func (check checkerFunc) Check(ctx context.Context) error {
	return check(ctx)
}

func TestLiveDoesNotDependOnDatabase(t *testing.T) {
	state := NewState(checkerFunc(func(context.Context) error {
		return errors.New("database unavailable")
	}), time.Second)
	handler := NewHandler(state, logging.New(&bytes.Buffer{}, "test", "dev", "unknown"))

	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if responseRecorder.Code != http.StatusOK || responseRecorder.Body.String() != "{\"status\":\"ok\"}" {
		t.Fatalf("存活响应不符合契约：code=%d body=%s", responseRecorder.Code, responseRecorder.Body.String())
	}
	if contentType := responseRecorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("存活响应内容类型不符合契约：%q", contentType)
	}
}

func TestReadyReturnsGenericFailureWithoutSecret(t *testing.T) {
	const secret = "postgres://name:secret@example.invalid/mail_suite"
	state := NewState(checkerFunc(func(context.Context) error {
		return errors.New(secret)
	}), time.Second)
	handler := NewHandler(state, logging.New(&bytes.Buffer{}, "test", "dev", "unknown"))

	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

	if responseRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际为 %d", responseRecorder.Code)
	}
	if body := responseRecorder.Body.String(); body != "{\"status\":\"unavailable\",\"code\":\"DEPENDENCY_UNAVAILABLE\"}" || strings.Contains(body, secret) {
		t.Fatalf("就绪失败响应不符合脱敏契约：%s", body)
	}
	if contentType := responseRecorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("就绪失败响应内容类型不符合契约：%q", contentType)
	}
}

func TestReadyChangesToUnavailableBeforeShutdown(t *testing.T) {
	state := NewState(checkerFunc(func(context.Context) error { return nil }), time.Second)
	handler := NewHandler(state, logging.New(&bytes.Buffer{}, "test", "dev", "unknown"))

	readyResponse := httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if readyResponse.Code != http.StatusOK {
		t.Fatalf("期望依赖可用时返回 200，实际为 %d", readyResponse.Code)
	}

	state.BeginShutdown()
	stoppingResponse := httptest.NewRecorder()
	handler.ServeHTTP(stoppingResponse, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if stoppingResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望停止前撤销就绪，实际为 %d", stoppingResponse.Code)
	}
}

func TestReadyLimitsDependencyCheckDuration(t *testing.T) {
	state := NewState(checkerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}), 20*time.Millisecond)
	handler := NewHandler(state, logging.New(&bytes.Buffer{}, "test", "dev", "unknown"))

	startedAt := time.Now()
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

	if responseRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望超时返回 503，实际为 %d", responseRecorder.Code)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("就绪检查未受超时约束：%s", elapsed)
	}
}
