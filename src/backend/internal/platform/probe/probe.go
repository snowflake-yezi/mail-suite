// Package probe 提供不泄露依赖细节的存活与就绪探针。
package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const dependencyUnavailableCode = "DEPENDENCY_UNAVAILABLE"

// Checker 表示一个必须满足的进程就绪条件。
type Checker interface {
	// Check 在上下文截止时间内返回依赖是否可用。
	Check(context.Context) error
}

// State 保存探针所需的依赖检查器和进程停止状态。
type State struct {
	checker      Checker
	probeTimeout time.Duration
	stopping     atomic.Bool
}

// NewState 创建探针状态，probeTimeout 限制每次就绪检查耗时。
func NewState(checker Checker, probeTimeout time.Duration) *State {
	return &State{checker: checker, probeTimeout: probeTimeout}
}

// BeginShutdown 在关闭监听器前立即撤销就绪状态。
func (state *State) BeginShutdown() {
	state.stopping.Store(true)
}

// NewHandler 创建仅包含内部健康端点的 Gin handler。
func NewHandler(state *State, logger *slog.Logger) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(requestLogger(logger), recovery(logger))
	router.GET("/health/live", state.live)
	router.GET("/health/ready", state.ready)
	return router
}

// response 是健康接口的稳定响应，不包含依赖或拓扑细节。
type response struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// live 只证明当前 HTTP 事件循环仍可提供服务。
func (state *State) live(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, response{Status: "ok"})
}

// ready 同时检查停止状态和必需的 PostgreSQL 依赖。
func (state *State) ready(ctx *gin.Context) {
	if state.stopping.Load() || state.checker == nil {
		writeUnavailable(ctx)
		return
	}

	checkContext, cancel := context.WithTimeout(ctx.Request.Context(), state.probeTimeout)
	defer cancel()
	if err := state.checker.Check(checkContext); err != nil {
		writeUnavailable(ctx)
		return
	}
	ctx.JSON(http.StatusOK, response{Status: "ok"})
}

// writeUnavailable 固定不可就绪响应，避免把底层错误传给调用方。
func writeUnavailable(ctx *gin.Context) {
	ctx.JSON(http.StatusServiceUnavailable, response{
		Status: "unavailable",
		Code:   dependencyUnavailableCode,
	})
}

// requestLogger 为每个请求建立受控 request_id 并记录有限元数据。
func requestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		startedAt := time.Now()
		requestID := normalizedRequestID(ctx.GetHeader("X-Request-ID"))
		ctx.Set("request_id", requestID)
		ctx.Header("X-Request-ID", requestID)

		ctx.Next()
		logger.InfoContext(
			ctx.Request.Context(),
			"HTTP 请求完成",
			"request_id", requestID,
			"method", ctx.Request.Method,
			"path", ctx.FullPath(),
			"status", ctx.Writer.Status(),
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	}
}

// recovery 将 panic 转为通用响应，避免 Gin 输出请求 header 或内部堆栈。
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(ctx *gin.Context, recovered any) {
		requestID, _ := ctx.Get("request_id")
		logger.ErrorContext(ctx.Request.Context(), "HTTP 请求异常终止", "request_id", requestID)
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, response{Status: "unavailable"})
	})
}

// normalizedRequestID 只接受有限字符的调用方标识，否则生成本地随机标识。
func normalizedRequestID(candidate string) string {
	if candidate != "" && len(candidate) <= 64 && strings.IndexFunc(candidate, func(value rune) bool {
		return !isRequestIDCharacter(value)
	}) == -1 {
		return candidate
	}

	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(buffer)
}

// isRequestIDCharacter 限制调用方 request_id 为可安全写入日志和响应头的 ASCII 字符。
func isRequestIDCharacter(value rune) bool {
	return (value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9') ||
		value == '-' || value == '_' || value == '.'
}
