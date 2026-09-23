// Package httpapi 将公共资源授权与请求安全边界接入业务 HTTP 路由。
package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

const contextKey = "authorization_context"

// sessionResolver 使用现有身份服务核对每次请求的服务端会话。
type sessionResolver interface {
	// ResolveSession 返回有效本地会话；匿名或过期时返回 nil。
	ResolveSession(context.Context, string) (*identity.SessionView, error)
}

// Boundary 保存身份服务和受信任主站，不缓存跨请求的主体或授权结果。
type Boundary struct {
	service       sessionResolver
	trustedOrigin string
}

// New 创建业务路由共用的授权入口，拒绝非 HTTPS 或携带路径的主站配置。
func New(service sessionResolver, trustedOrigin string) (*Boundary, error) {
	parsed, err := url.ParseRequestURI(trustedOrigin)
	if service == nil || err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("授权 HTTP handler 配置无效")
	}
	return &Boundary{service: service, trustedOrigin: trustedOrigin}, nil
}

// Protect 在业务 handler 前核对会话、账号类型与写请求边界；有外部副作用的 POST 应要求幂等键。
func (boundary *Boundary) Protect(accountType identity.AccountType, requireIdempotency bool) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		ctx.Header("Cache-Control", "no-store")
		token, _ := ctx.Cookie(identity.SessionCookieName)
		view, err := boundary.service.ResolveSession(ctx.Request.Context(), token)
		if err != nil {
			WriteError(ctx, err)
			return
		}
		if view == nil {
			WriteError(ctx, authorization.NewError(authorization.ErrorCodeAuthRequired))
			return
		}
		authorized, err := authorization.NewContext(view, ctx.GetString("request_id"))
		if err != nil {
			WriteError(ctx, err)
			return
		}
		if accountType != authorized.AccountType() {
			WriteError(ctx, authorization.NewError(authorization.ErrorCodeAuthForbidden))
			return
		}
		if accountType == identity.AccountTypeAdministrator {
			if err := authorized.RequireAdministrator(); err != nil {
				WriteError(ctx, err)
				return
			}
		} else if accountType != identity.AccountTypeMailbox {
			WriteError(ctx, authorization.NewError(authorization.ErrorCodeAuthForbidden))
			return
		}

		switch ctx.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if ctx.Request.ContentLength > 0 || len(ctx.Request.TransferEncoding) > 0 ||
				(ctx.Request.Body != nil && ctx.Request.Body != http.NoBody) {
				WriteError(ctx, authorization.NewError(authorization.ErrorCodeInvalidArgument))
				return
			}
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			authorized, err = authorization.ValidateMutationRequest(ctx.Request, boundary.trustedOrigin, authorized, requireIdempotency)
			if err != nil {
				WriteError(ctx, err)
				return
			}
			if err := boundJSONBody(ctx.Request); err != nil {
				WriteError(ctx, err)
				return
			}
		default:
			WriteError(ctx, authorization.NewError(authorization.ErrorCodeInvalidArgument))
			return
		}
		ctx.Set(contextKey, authorized)
		ctx.Next()
	}
}

// Context 从已通过 Protect 的请求中取回只读授权上下文。
func Context(ctx *gin.Context) (authorization.Context, bool) {
	value, exists := ctx.Get(contextKey)
	if !exists {
		return authorization.Context{}, false
	}
	authorized, ok := value.(authorization.Context)
	return authorized, ok
}

func boundJSONBody(request *http.Request) error {
	if request.Body == nil {
		return nil
	}
	input := request.Body
	defer func() { _ = input.Close() }()
	body, err := io.ReadAll(io.LimitReader(input, authorization.DefaultMaxJSONBodyBytes+1))
	if err != nil || int64(len(body)) > authorization.DefaultMaxJSONBodyBytes {
		return authorization.NewError(authorization.ErrorCodeInvalidArgument)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return nil
}

// WriteError 将受控错误码映射为固定状态和脱敏 envelope，其他错误一律隐藏细节。
func WriteError(ctx *gin.Context, err error) {
	code := authorization.ErrorCodePersistenceUnavailable
	var authorizedError *authorization.Error
	var identityError *identity.Error
	if errors.As(err, &authorizedError) && authorizedError != nil {
		code = authorizedError.Code
	} else if errors.As(err, &identityError) && identityError != nil {
		switch identityError.Code {
		case identity.ErrorCodeAuthRequired:
			code = authorization.ErrorCodeAuthRequired
		case identity.ErrorCodeAuthForbidden:
			code = authorization.ErrorCodeAuthForbidden
		}
	}
	status := http.StatusServiceUnavailable
	switch code {
	case authorization.ErrorCodeAuthRequired:
		status = http.StatusUnauthorized
	case authorization.ErrorCodeAuthForbidden, authorization.ErrorCodeCSRFInvalid:
		status = http.StatusForbidden
	case authorization.ErrorCodeInvalidArgument:
		status = http.StatusBadRequest
	case authorization.ErrorCodeNotFound, authorization.ErrorCodeOperationNotFound:
		status = http.StatusNotFound
	case authorization.ErrorCodeIdempotencyConflict:
		status = http.StatusConflict
	case authorization.ErrorCodeUpstreamTimeout:
		status = http.StatusGatewayTimeout
	case authorization.ErrorCodeUpstreamUnavailable, authorization.ErrorCodePersistenceUnavailable:
		status = http.StatusServiceUnavailable
	default:
		code = authorization.ErrorCodePersistenceUnavailable
	}
	ctx.Header("Cache-Control", "no-store")
	ctx.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    authorization.NewError(code).Message,
		"request_id": ctx.GetString("request_id"),
	}})
}
