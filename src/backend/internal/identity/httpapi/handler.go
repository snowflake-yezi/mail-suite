// Package httpapi 将身份领域服务映射为同源浏览器认证 HTTP 契约。
package httpapi

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

const (
	maximumStateBytes             = 2048
	maximumAuthorizationCodeBytes = 4096
	maximumProviderErrorBytes     = 256
)

// Handler 提供登录、callback、会话查询和本地退出路由。
type Handler struct {
	service       authService
	trustedOrigin string
}

// authService 是 HTTP adapter 使用的最小身份应用服务边界。
type authService interface {
	// StartLogin 创建一次性登录流程和 provider 跳转。
	StartLogin(context.Context, string, identity.RequestMetadata) (identity.LoginStart, error)
	// CompleteLogin 校验 callback 并创建本地会话。
	CompleteLogin(context.Context, identity.CallbackInput) (identity.CallbackResult, error)
	// RejectLogin 消费 provider 拒绝的一次性流程。
	RejectLogin(context.Context, string, string, identity.RequestMetadata) error
	// ResolveSession 查询当前浏览器会话。
	ResolveSession(context.Context, string) (*identity.SessionView, error)
	// Logout 校验 CSRF 并撤销当前会话。
	Logout(context.Context, string, string, identity.RequestMetadata) error
}

// New 创建只信任配置主站 origin 的认证 HTTP handler。
func New(service authService, trustedOrigin string) (*Handler, error) {
	parsed, err := url.ParseRequestURI(trustedOrigin)
	if service == nil || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" {
		return nil, errors.New("认证 HTTP handler 配置无效")
	}
	return &Handler{service: service, trustedOrigin: trustedOrigin}, nil
}

// RegisterRoutes 在 API engine 上注册当前增量批准的四个认证端点。
func (handler *Handler) RegisterRoutes(router *gin.Engine) {
	router.GET("/api/v1/auth/login", handler.login)
	router.GET("/api/v1/auth/callback", handler.callback)
	router.GET("/api/v1/session", handler.session)
	router.POST("/api/v1/auth/logout", handler.logout)
}

// login 创建一次性流程 Cookie 并跳转外部 OIDC provider。
func (handler *Handler) login(ctx *gin.Context) {
	ctx.Header("Cache-Control", "no-store")
	start, err := handler.service.StartLogin(ctx.Request.Context(), ctx.Query("return_to"), requestMetadata(ctx))
	if err != nil {
		writeError(ctx, err)
		return
	}
	setSecureCookie(ctx, identity.AuthFlowCookieName, start.BrowserCookie, int(identity.DefaultAuthFlowLifetime.Seconds()), start.ExpiresAt)
	ctx.Redirect(http.StatusFound, start.AuthorizationURL)
}

// callback 校验参数互斥、清理临时 Cookie，并完成或拒绝一次性登录流程。
func (handler *Handler) callback(ctx *gin.Context) {
	ctx.Header("Cache-Control", "no-store")
	state := ctx.Query("state")
	code := ctx.Query("code")
	providerError := ctx.Query("error")
	browserCookie, _ := ctx.Cookie(identity.AuthFlowCookieName)
	clearSecureCookie(ctx, identity.AuthFlowCookieName)
	if state == "" || len(state) > maximumStateBytes ||
		(code == "") == (providerError == "") ||
		len(code) > maximumAuthorizationCodeBytes || len(providerError) > maximumProviderErrorBytes {
		writeError(ctx, handler.service.RejectLogin(ctx.Request.Context(), state, browserCookie, requestMetadata(ctx)))
		return
	}
	metadata := requestMetadata(ctx)
	if providerError != "" {
		writeError(ctx, handler.service.RejectLogin(ctx.Request.Context(), state, browserCookie, metadata))
		return
	}
	result, err := handler.service.CompleteLogin(ctx.Request.Context(), identity.CallbackInput{
		State:         state,
		BrowserCookie: browserCookie,
		Code:          code,
		Metadata:      metadata,
	})
	if err != nil {
		writeError(ctx, err)
		return
	}
	setSecureCookie(
		ctx,
		identity.SessionCookieName,
		result.SessionToken,
		int(identity.DefaultSessionAbsoluteTimeout.Seconds()),
		result.ExpiresAt,
	)
	ctx.Redirect(http.StatusFound, result.RedirectTo)
}

// session 返回匿名判别或当前状态重新授权后的最小会话摘要。
func (handler *Handler) session(ctx *gin.Context) {
	ctx.Header("Cache-Control", "no-store")
	sessionToken, _ := ctx.Cookie(identity.SessionCookieName)
	view, err := handler.service.ResolveSession(ctx.Request.Context(), sessionToken)
	if err != nil {
		writeError(ctx, err)
		return
	}
	if view == nil {
		ctx.JSON(http.StatusOK, anonymousSessionResponse{Authenticated: false})
		return
	}
	permissions := append([]string{}, view.Session.Permissions...)
	slices.Sort(permissions)
	response := authenticatedSessionResponse{
		Authenticated: true,
		Principal: principalResponse{
			ID:          view.Session.Principal.ID.String(),
			DisplayName: view.Session.Principal.DisplayName,
		},
		AccountType: string(view.Session.Principal.AccountType),
		TenantID:    view.Session.Principal.TenantID.String(),
		Permissions: permissions,
		ExpiresAt:   view.Session.ExpiresAt().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		CSRFToken:   view.CSRFToken,
	}
	if view.Session.Principal.Mailbox != nil {
		response.Mailbox = &mailboxResponse{
			ID:      view.Session.Principal.Mailbox.ID.String(),
			Address: view.Session.Principal.Mailbox.Address,
		}
	}
	ctx.JSON(http.StatusOK, response)
}

// logout 在可信同源请求中校验 CSRF，并在服务端撤销成功后清理 Cookie。
func (handler *Handler) logout(ctx *gin.Context) {
	ctx.Header("Cache-Control", "no-store")
	if !validLogoutContent(ctx.Request) || !handler.trustedBrowserRequest(ctx.Request) {
		writeError(ctx, identity.NewError(identity.ErrorCodeCSRFInvalid))
		return
	}
	sessionToken, _ := ctx.Cookie(identity.SessionCookieName)
	if err := handler.service.Logout(
		ctx.Request.Context(),
		sessionToken,
		ctx.GetHeader("X-CSRF-Token"),
		requestMetadata(ctx),
	); err != nil {
		writeError(ctx, err)
		return
	}
	clearSecureCookie(ctx, identity.SessionCookieName)
	ctx.Status(http.StatusNoContent)
}

// validLogoutContent 只接受无请求体的 POST，并限制显式 Content-Type 为 JSON。
func validLogoutContent(request *http.Request) bool {
	if request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		return false
	}
	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

// trustedBrowserRequest 要求 Origin 精确匹配，缺少时只接受同源 HTTPS Referer 回退。
func (handler *Handler) trustedBrowserRequest(request *http.Request) bool {
	if origin := request.Header.Get("Origin"); origin != "" {
		return origin == handler.trustedOrigin
	}
	referer := request.Header.Get("Referer")
	parsed, err := url.ParseRequestURI(referer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return parsed.Scheme+"://"+parsed.Host == handler.trustedOrigin
}

// requestMetadata 只把 request ID 与待摘要来源材料交给领域服务。
func requestMetadata(ctx *gin.Context) identity.RequestMetadata {
	requestID, _ := ctx.Get("request_id")
	return identity.RequestMetadata{
		RequestID: valueString(requestID),
		Source:    ctx.ClientIP() + "\x00" + ctx.Request.UserAgent(),
	}
}

// setSecureCookie 写入符合 __Host- 前缀要求的浏览器 Cookie。

func setSecureCookie(ctx *gin.Context, name, value string, maxAge int, expiresAt time.Time) {
	http.SetCookie(ctx.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt.UTC(),
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSecureCookie 使用相同安全属性立即清理浏览器 Cookie。
func clearSecureCookie(ctx *gin.Context, name string) {
	http.SetCookie(ctx.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// writeError 将领域错误稳定映射为不泄露 provider 或数据库细节的统一 JSON。
func writeError(ctx *gin.Context, err error) {
	code := identity.ErrorCodePersistenceUnavailable
	message := identity.NewError(code).Message
	status := http.StatusServiceUnavailable
	var identityError *identity.Error
	if errors.As(err, &identityError) {
		code = identityError.Code
		message = identityError.Message
		switch code {
		case identity.ErrorCodeInvalidRequest, identity.ErrorCodeInvalidReturnTo, identity.ErrorCodeAuthFlowInvalid:
			status = http.StatusBadRequest
		case identity.ErrorCodeAuthForbidden, identity.ErrorCodeCSRFInvalid:
			status = http.StatusForbidden
		case identity.ErrorCodeAuthRequired:
			status = http.StatusUnauthorized
		default:
			status = http.StatusServiceUnavailable
		}
	}
	requestID, _ := ctx.Get("request_id")
	ctx.AbortWithStatusJSON(status, errorResponse{Error: errorDetail{
		Code:      string(code),
		Message:   message,
		RequestID: valueString(requestID),
	}})
}

// valueString 只提取中间件生成的受控字符串值。
func valueString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// anonymousSessionResponse 是不泄露旧 Cookie 状态的固定匿名响应。
type anonymousSessionResponse struct {
	Authenticated bool `json:"authenticated"`
}

// authenticatedSessionResponse 是邮箱与管理账号共享的会话响应主体。
type authenticatedSessionResponse struct {
	Authenticated bool              `json:"authenticated"`
	Principal     principalResponse `json:"principal"`
	AccountType   string            `json:"account_type"`
	TenantID      string            `json:"tenant_id"`
	Mailbox       *mailboxResponse  `json:"mailbox,omitempty"`
	Permissions   []string          `json:"permissions"`
	ExpiresAt     string            `json:"expires_at"`
	CSRFToken     string            `json:"csrf_token"`
}

// principalResponse 是前端展示所需的最小本地主体快照。
type principalResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// mailboxResponse 是邮箱账号会话固定绑定的唯一邮箱摘要。
type mailboxResponse struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

// errorResponse 包装项目统一认证错误结构。
type errorResponse struct {
	Error errorDetail `json:"error"`
}

// errorDetail 保存公开错误码、通用消息和请求关联标识。
type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}
