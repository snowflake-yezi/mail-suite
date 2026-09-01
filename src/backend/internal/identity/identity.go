// Package identity 定义外部 OIDC 身份、本地账号映射和服务端会话的业务边界。
package identity

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	// SessionCookieName 是浏览器持有的不透明服务端会话 Cookie 名称。
	SessionCookieName = "__Host-mail_suite_session"
	// AuthFlowCookieName 是 OIDC 发起与 callback 之间的临时浏览器关联 Cookie 名称。
	AuthFlowCookieName = "__Host-mail_suite_auth_flow"
	// PermissionPortalAdminAccess 是进入管理门户所需的最小本地权限。
	PermissionPortalAdminAccess = "portal.admin.access"
	// DefaultAuthFlowLifetime 是一次性 OIDC 登录流程允许存在的最长时间。
	DefaultAuthFlowLifetime = 10 * time.Minute
	// DefaultSessionIdleTimeout 是有效会话的默认空闲超时。
	DefaultSessionIdleTimeout = 30 * time.Minute
	// DefaultSessionAbsoluteTimeout 是有效会话不可延长的绝对超时。
	DefaultSessionAbsoluteTimeout = 8 * time.Hour
	// DefaultSessionTouchInterval 限制最近访问时间的数据库写入频率。
	DefaultSessionTouchInterval = 5 * time.Minute
)

// AccountType 表示由本地控制数据唯一确定的产品账号类型。
type AccountType string

const (
	// AccountTypeMailbox 表示只能进入会话绑定邮箱的邮箱账号。
	AccountTypeMailbox AccountType = "mailbox"
	// AccountTypeAdministrator 表示只能进入管理控制台的管理账号。
	AccountTypeAdministrator AccountType = "administrator"
)

// Valid 判断账号类型是否属于公开契约允许的稳定值。
func (accountType AccountType) Valid() bool {
	return accountType == AccountTypeMailbox || accountType == AccountTypeAdministrator
}

// ErrorCode 是可由 HTTP adapter 稳定映射且不泄露身份存在性的认证错误标识。
type ErrorCode string

const (
	// ErrorCodeInvalidRequest 表示调用参数不满足认证领域约束。
	ErrorCodeInvalidRequest ErrorCode = "INVALID_REQUEST"
	// ErrorCodeAuthRequired 表示当前请求没有可用的本地会话。
	ErrorCodeAuthRequired ErrorCode = "AUTH_REQUIRED"
	// ErrorCodeAuthForbidden 隐藏主体不存在、停用和缺少产品权限等内部差异。
	ErrorCodeAuthForbidden ErrorCode = "AUTH_FORBIDDEN"
	// ErrorCodeAuthFlowInvalid 表示一次性登录流程不存在、过期、已消费或不匹配。
	ErrorCodeAuthFlowInvalid ErrorCode = "AUTH_FLOW_INVALID"
	// ErrorCodePersistenceUnavailable 表示数据库无法完成或确认认证操作。
	ErrorCodePersistenceUnavailable ErrorCode = "AUTH_SERVICE_UNAVAILABLE"
)

// Error 保存稳定错误码和不包含底层实现细节的安全消息。
type Error struct {
	// Code 是 HTTP adapter 和测试可判断的稳定认证错误码。
	Code ErrorCode
	// Message 是可安全记录或返回给调用方的通用消息。
	Message string
}

// Error 返回不包含数据库、OIDC 响应或凭据细节的安全消息。
func (identityError *Error) Error() string {
	return identityError.Message
}

// NewError 根据稳定错误码创建认证领域错误。
func NewError(code ErrorCode) *Error {
	messages := map[ErrorCode]string{
		ErrorCodeInvalidRequest:         "认证请求无效",
		ErrorCodeAuthRequired:           "需要登录",
		ErrorCodeAuthForbidden:          "当前账号无法访问 Mail Suite",
		ErrorCodeAuthFlowInvalid:        "登录请求已失效，请重新登录",
		ErrorCodePersistenceUnavailable: "登录服务暂时不可用",
	}
	message, found := messages[code]
	if !found {
		code = ErrorCodePersistenceUnavailable
		message = messages[code]
	}
	return &Error{Code: code, Message: message}
}

// HasErrorCode 判断错误链中是否包含指定认证错误码。
func HasErrorCode(err error, code ErrorCode) bool {
	var identityError *Error
	return errors.As(err, &identityError) && identityError.Code == code
}

// Mailbox 表示邮箱账号会话中唯一允许暴露的邮箱摘要。
type Mailbox struct {
	// ID 是当前主体固定绑定的邮箱稳定标识。
	ID uuid.UUID
	// Address 是由受控邮箱本地部分和域名组合的展示地址。
	Address string
}

// Principal 表示通过外部身份映射并通过当前状态校验的本地主体。
type Principal struct {
	// ID 是本地认证主体稳定标识。
	ID uuid.UUID
	// TenantID 是主体所属的一级授权边界。
	TenantID uuid.UUID
	// AccountType 唯一决定邮箱端或管理端落点。
	AccountType AccountType
	// DisplayName 是不参与身份或授权判断的展示名称快照。
	DisplayName string
	// Mailbox 仅对邮箱账号存在，并固定为同租户唯一邮箱。
	Mailbox *Mailbox
}

// AuthFlowToCreate 保存创建一次性登录流程所需的脱敏或加密数据。
type AuthFlowToCreate struct {
	// ID 是登录流程内部稳定标识。
	ID uuid.UUID
	// StateDigest 是带服务端 pepper 的 state 摘要。
	StateDigest [32]byte
	// BrowserCookieDigest 是临时浏览器关联 Cookie 的摘要。
	BrowserCookieDigest [32]byte
	// NonceDigest 是 OIDC nonce 的摘要。
	NonceDigest [32]byte
	// PKCEVerifierCiphertext 是经批准密钥加密的 verifier 密文。
	PKCEVerifierCiphertext []byte
	// EncryptionKeyID 是解密 verifier 的非秘密密钥版本标识。
	EncryptionKeyID string
	// ReturnTo 是经过规范化和账号端白名单校验的站内路径。
	ReturnTo string
	// CreatedAt 是流程创建时间。
	CreatedAt time.Time
	// ExpiresAt 是不晚于创建后十分钟的流程过期时间。
	ExpiresAt time.Time
}

// ConsumedAuthFlow 表示已被 callback 原子消费且不能重放的登录流程。
type ConsumedAuthFlow struct {
	// ID 是被消费流程的内部稳定标识。
	ID uuid.UUID
	// NonceDigest 用于校验 ID token nonce。
	NonceDigest [32]byte
	// PKCEVerifierCiphertext 是等待 OIDC token exchange 使用的 verifier 密文。
	PKCEVerifierCiphertext []byte
	// EncryptionKeyID 是解密 verifier 所需的密钥版本标识。
	EncryptionKeyID string
	// ReturnTo 是登录成功后仍需按账号类型复核的站内路径。
	ReturnTo string
	// ExpiresAt 是流程原始过期时间。
	ExpiresAt time.Time
	// ConsumedAt 是本次 callback 成功取得流程所有权的时间。
	ConsumedAt time.Time
}

// SessionToCreate 表示 OIDC 和本地主体校验完成后待持久化的新会话。
type SessionToCreate struct {
	// ID 是服务端内部会话稳定标识。
	ID uuid.UUID
	// SessionDigest 是不透明 Cookie 的带 pepper 摘要。
	SessionDigest [32]byte
	// PrincipalID 是会话创建后不可切换的本地主体标识。
	PrincipalID uuid.UUID
	// OIDCIssuer 是已经校验的签发方标识。
	OIDCIssuer string
	// OIDCSubject 是已经校验的不透明外部主体标识。
	OIDCSubject string
	// OIDCSessionID 是可选 sid，用于 backchannel 精确撤销。
	OIDCSessionID string
	// CSRFDigest 是与会话绑定的 CSRF nonce 摘要。
	CSRFDigest [32]byte
	// CreatedAt 是会话创建和绝对生命周期起点。
	CreatedAt time.Time
	// IdleExpiresAt 是初始空闲过期时间。
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt 是不可延长的绝对过期时间。
	AbsoluteExpiresAt time.Time
}

// CreatedSession 表示已成功持久化的新会话生命周期。
type CreatedSession struct {
	// ID 是服务端内部会话稳定标识。
	ID uuid.UUID
	// PrincipalID 是会话固定绑定的本地主体标识。
	PrincipalID uuid.UUID
	// CreatedAt 是数据库确认的会话创建时间。
	CreatedAt time.Time
	// IdleExpiresAt 是数据库确认的初始空闲过期时间。
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt 是数据库确认的绝对过期时间。
	AbsoluteExpiresAt time.Time
}

// Session 表示每次请求重新校验主体、租户、邮箱和权限后的当前会话。
type Session struct {
	// ID 是服务端内部会话稳定标识。
	ID uuid.UUID
	// Principal 是当前状态有效的本地账号映射。
	Principal Principal
	// Permissions 是按稳定名称排序的当前本地权限快照。
	Permissions []string
	// CreatedAt 是会话创建时间。
	CreatedAt time.Time
	// LastSeenAt 是最近一次被限频持久化的有效访问时间。
	LastSeenAt time.Time
	// IdleExpiresAt 是本次读取后有效的空闲过期时间。
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt 是不可延长的绝对过期时间。
	AbsoluteExpiresAt time.Time
	// CSRFDigest 是数据库保存的当前 CSRF nonce 摘要。
	CSRFDigest [32]byte
}

// ExpiresAt 返回空闲过期和绝对过期时间中的较早值。
func (session Session) ExpiresAt() time.Time {
	if session.IdleExpiresAt.Before(session.AbsoluteExpiresAt) {
		return session.IdleExpiresAt
	}
	return session.AbsoluteExpiresAt
}

// RevocationReason 是可审计且不包含凭据的稳定会话撤销原因。
type RevocationReason string

const (
	// RevocationReasonLogout 表示浏览器主动退出。
	RevocationReasonLogout RevocationReason = "logout"
	// RevocationReasonRotation 表示安全边界变化触发会话标识轮换。
	RevocationReasonRotation RevocationReason = "rotation"
	// RevocationReasonBackchannelSID 表示按 OIDC sid 精确撤销。
	RevocationReasonBackchannelSID RevocationReason = "backchannel_sid"
	// RevocationReasonBackchannelSubject 表示按 issuer 与 subject 撤销全部设备会话。
	RevocationReasonBackchannelSubject RevocationReason = "backchannel_subject"
	// RevocationReasonPrincipalDisabled 表示本地主体被停用。
	RevocationReasonPrincipalDisabled RevocationReason = "principal_disabled"
	// RevocationReasonPermissionRevoked 表示维持产品入口所需的权限被撤销。
	RevocationReasonPermissionRevoked RevocationReason = "permission_revoked"
)

// Valid 判断撤销原因是否属于认证契约允许的稳定值。
func (reason RevocationReason) Valid() bool {
	switch reason {
	case RevocationReasonLogout,
		RevocationReasonRotation,
		RevocationReasonBackchannelSID,
		RevocationReasonBackchannelSubject,
		RevocationReasonPrincipalDisabled,
		RevocationReasonPermissionRevoked:
		return true
	default:
		return false
	}
}

// AuditEvent 表示不包含 token、Cookie、完整 claim 或密码的认证审计输入。
type AuditEvent struct {
	// ID 是认证审计事件稳定标识。
	ID uuid.UUID
	// PrincipalID 在无法安全映射本地主体时为空。
	PrincipalID *uuid.UUID
	// Action 是由认证契约定义的稳定动作名称。
	Action string
	// Result 是 succeeded 或 failed。
	Result string
	// FailureCategory 仅在失败时保存稳定脱敏类别。
	FailureCategory string
	// SourceDigest 是来源信息的不可逆摘要。
	SourceDigest [32]byte
	// RequestID 是受控请求关联标识。
	RequestID string
	// OccurredAt 是服务端确认动作结果的时间。
	OccurredAt time.Time
}

// Repository 定义 OIDC adapter、会话服务与 PostgreSQL 之间的认证持久化边界。
type Repository interface {
	// CreateAuthFlow 创建最长十分钟的一次性登录流程。
	CreateAuthFlow(context.Context, AuthFlowToCreate) error
	// ConsumeAuthFlow 按 state 与浏览器关联摘要原子消费登录流程。
	ConsumeAuthFlow(context.Context, [32]byte, [32]byte, time.Time) (ConsumedAuthFlow, error)
	// ResolvePrincipal 按 issuer 与 subject 解析当前有效的唯一产品账号。
	ResolvePrincipal(context.Context, string, string) (Principal, error)
	// CreateSession 创建已完成 OIDC 和本地授权校验的服务端会话。
	CreateSession(context.Context, SessionToCreate) (CreatedSession, error)
	// ResolveSession 按 Cookie 摘要读取并刷新当前有效会话。
	ResolveSession(context.Context, [32]byte, time.Time) (Session, error)
	// RevokeSession 按 Cookie 摘要幂等撤销当前会话。
	RevokeSession(context.Context, [32]byte, time.Time, RevocationReason) error
	// RevokeByOIDCSessionID 按 issuer 与 sid 幂等撤销匹配设备会话。
	RevokeByOIDCSessionID(context.Context, string, string, time.Time) (int64, error)
	// RevokeByOIDCSubject 按 issuer 与 subject 幂等撤销全部设备会话。
	RevokeByOIDCSubject(context.Context, string, string, time.Time) (int64, error)
	// RecordAudit 保存不含认证凭据和原始 claim 的安全审计事件。
	RecordAudit(context.Context, AuditEvent) error
}
