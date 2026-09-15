// Package authorization 提供业务 API 共用的服务端授权上下文与请求边界。
package authorization

import (
	"crypto/subtle"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

const (
	// PermissionPortalAdminAccess 是管理业务 API 的最小入口权限。
	PermissionPortalAdminAccess = identity.PermissionPortalAdminAccess
	// DefaultMaxJSONBodyBytes 限制公共业务请求体，避免 handler 在解析前被大请求占满内存。
	DefaultMaxJSONBodyBytes int64 = 1 << 20
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// ErrorCode 是授权边界向 HTTP adapter 暴露的稳定错误标识。
type ErrorCode string

const (
	// ErrorCodeAuthRequired 表示没有可用的服务端会话。
	ErrorCodeAuthRequired ErrorCode = "AUTH_REQUIRED"
	// ErrorCodeAuthForbidden 表示主体已登录但没有访问权限。
	ErrorCodeAuthForbidden ErrorCode = "AUTH_FORBIDDEN"
	// ErrorCodeCSRFInvalid 表示请求来源或会话 CSRF nonce 不匹配。
	ErrorCodeCSRFInvalid ErrorCode = "CSRF_INVALID"
	// ErrorCodeInvalidArgument 表示请求头或公共参数不符合边界。
	ErrorCodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	// ErrorCodeNotFound 隐藏跨租户或跨邮箱资源是否存在。
	ErrorCodeNotFound ErrorCode = "NOT_FOUND"
	// ErrorCodeIdempotencyConflict 表示幂等键绑定了不同请求。
	ErrorCodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT"
)

// Error 是不包含数据库、Cookie 或凭据细节的公开授权错误。
type Error struct {
	// Code 是上层 HTTP adapter 可稳定分支的错误码。
	Code ErrorCode
	// Message 是可直接返回给浏览器的通用中文消息。
	Message string
}

// Error 返回脱敏后的错误消息。
func (authorizationError *Error) Error() string {
	return authorizationError.Message
}

// NewError 创建指定错误码的稳定错误。
func NewError(code ErrorCode) *Error {
	messages := map[ErrorCode]string{
		ErrorCodeAuthRequired:        "需要登录",
		ErrorCodeAuthForbidden:       "当前账号无权访问此资源",
		ErrorCodeCSRFInvalid:         "请求验证失败",
		ErrorCodeInvalidArgument:     "请求参数无效",
		ErrorCodeNotFound:            "资源不存在",
		ErrorCodeIdempotencyConflict: "幂等键已用于不同请求",
	}
	message, ok := messages[code]
	if !ok {
		code = ErrorCodeAuthForbidden
		message = messages[code]
	}
	return &Error{Code: code, Message: message}
}

// Context 保存一次请求已经由服务端确认的主体和资源边界。
type Context struct {
	principalID uuid.UUID
	tenantID    uuid.UUID
	accountType identity.AccountType
	mailboxID   uuid.UUID
	permissions []string
	csrfToken   string
	requestID   string
}

// NewContext 将当前有效会话转换为不可变授权上下文。
func NewContext(view *identity.SessionView, requestID string) (Context, error) {
	if view == nil || view.Session.Principal.ID == uuid.Nil || view.Session.Principal.TenantID == uuid.Nil ||
		!view.Session.Principal.AccountType.Valid() || strings.TrimSpace(requestID) == "" ||
		strings.TrimSpace(view.CSRFToken) == "" {
		return Context{}, NewError(ErrorCodeAuthRequired)
	}
	principal := view.Session.Principal
	if principal.AccountType == identity.AccountTypeMailbox {
		if principal.Mailbox == nil || principal.Mailbox.ID == uuid.Nil {
			return Context{}, NewError(ErrorCodeAuthForbidden)
		}
	} else if principal.Mailbox != nil {
		return Context{}, NewError(ErrorCodeAuthForbidden)
	}
	permissions := append([]string(nil), view.Session.Permissions...)
	sort.Strings(permissions)
	return Context{
		principalID: principal.ID,
		tenantID:    principal.TenantID,
		accountType: principal.AccountType,
		mailboxID:   mailboxID(principal.Mailbox),
		permissions: permissions,
		csrfToken:   view.CSRFToken,
		requestID:   strings.TrimSpace(requestID),
	}, nil
}

// PrincipalID 返回服务端确认的主体标识。
func (context Context) PrincipalID() uuid.UUID { return context.principalID }

// TenantID 返回服务端确认的租户标识。
func (context Context) TenantID() uuid.UUID { return context.tenantID }

// AccountType 返回服务端确认的账号类型。
func (context Context) AccountType() identity.AccountType { return context.accountType }

// MailboxID 返回会话绑定的邮箱标识；管理员上下文返回 uuid.Nil。
func (context Context) MailboxID() uuid.UUID { return context.mailboxID }

// RequestID 返回当前请求的关联标识。
func (context Context) RequestID() string { return context.requestID }

// Permissions 返回排序后的权限副本，防止调用方修改上下文。
func (context Context) Permissions() []string { return append([]string(nil), context.permissions...) }

// HasPermission 判断当前主体是否拥有指定的本地权限。
func (context Context) HasPermission(permission string) bool {
	index := sort.SearchStrings(context.permissions, permission)
	return index < len(context.permissions) && context.permissions[index] == permission
}

// VerifyCSRF 使用常量时间比较校验会话绑定的 CSRF nonce。
func (context Context) VerifyCSRF(token string) bool {
	if token == "" || context.csrfToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(context.csrfToken)) == 1
}

// RequireAdministrator 要求当前上下文是具备管理入口权限的 administrator。
func (context Context) RequireAdministrator() error {
	if context.accountType != identity.AccountTypeAdministrator || !context.HasPermission(PermissionPortalAdminAccess) {
		return NewError(ErrorCodeAuthForbidden)
	}
	return nil
}

// RequireMailbox 要求当前上下文是绑定指定邮箱的 mailbox 主体。
func (context Context) RequireMailbox(resourceMailboxID uuid.UUID) error {
	if context.accountType != identity.AccountTypeMailbox || resourceMailboxID == uuid.Nil || context.mailboxID != resourceMailboxID {
		return NewError(ErrorCodeNotFound)
	}
	return nil
}

// Resource 描述业务 handler 需要重新核对的控制面资源归属链。
type Resource struct {
	// TenantID 是资源所属租户。
	TenantID uuid.UUID
	// MailboxID 是可选的资源所属邮箱。
	MailboxID uuid.UUID
}

// RequireResource 按账号类型和控制面归属链校验资源访问权。
func (context Context) RequireResource(resource Resource) error {
	if resource.TenantID == uuid.Nil || context.tenantID != resource.TenantID {
		return NewError(ErrorCodeNotFound)
	}
	if context.accountType == identity.AccountTypeAdministrator {
		return context.RequireAdministrator()
	}
	return context.RequireMailbox(resource.MailboxID)
}

// ValidateMutationRequest 校验公共写请求边界，不执行任何领域副作用。
func ValidateMutationRequest(request *http.Request, trustedOrigin string, context Context, requireIdempotency bool) error {
	if request == nil || !isMutation(request.Method) {
		return NewError(ErrorCodeInvalidArgument)
	}
	if request.ContentLength > DefaultMaxJSONBodyBytes {
		return NewError(ErrorCodeInvalidArgument)
	}
	if !trustedBrowserRequest(request, trustedOrigin) || !context.VerifyCSRF(request.Header.Get("X-CSRF-Token")) {
		return NewError(ErrorCodeCSRFInvalid)
	}
	if err := validateContentType(request); err != nil {
		return err
	}
	if requireIdempotency && !validIdempotencyKey(request.Header.Get("Idempotency-Key")) {
		return NewError(ErrorCodeInvalidArgument)
	}
	return nil
}

func mailboxID(mailbox *identity.Mailbox) uuid.UUID {
	if mailbox == nil {
		return uuid.Nil
	}
	return mailbox.ID
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func trustedBrowserRequest(request *http.Request, trustedOrigin string) bool {
	parsedOrigin, err := url.ParseRequestURI(trustedOrigin)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Host == "" || parsedOrigin.Path != "" || parsedOrigin.RawQuery != "" {
		return false
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		return origin == trustedOrigin
	}
	referer, err := url.ParseRequestURI(request.Header.Get("Referer"))
	return err == nil && referer.Scheme == parsedOrigin.Scheme && referer.Host == parsedOrigin.Host &&
		referer.Scheme+"://"+referer.Host == trustedOrigin
}

func validateContentType(request *http.Request) error {
	contentType := request.Header.Get("Content-Type")
	if contentType == "" && request.ContentLength == 0 {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return NewError(ErrorCodeInvalidArgument)
	}
	return nil
}

func validIdempotencyKey(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 16 && len(value) <= 128 && idempotencyKeyPattern.MatchString(value)
}

// IsErrorCode 判断错误链中是否包含指定授权错误。
func IsErrorCode(err error, code ErrorCode) bool {
	var authorizationError *Error
	return errors.As(err, &authorizationError) && authorizationError.Code == code
}
