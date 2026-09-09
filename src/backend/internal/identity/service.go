package identity

import (
	"context"
	"crypto/hmac"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultMailboxLanding = "/mail/inbox"
	defaultAdminLanding   = "/admin/overview"
)

// OIDCIdentity 保存 provider 已完成协议校验后的最小身份认证证据。
type OIDCIdentity struct {
	// Issuer 是与 discovery 配置精确匹配的签发方。
	Issuer string
	// Subject 是 provider 签发且不可为空的稳定主体标识。
	Subject string
	// SessionID 是可选 OIDC sid，用于后续精确撤销。
	SessionID string
	// Nonce 是 ID token 中等待与一次性流程摘要比对的随机值。
	Nonce string
	// ACR 是 provider 声明的认证上下文等级。
	ACR string
	// AMR 是 provider 声明的认证方式集合。
	AMR []string
}

// OIDCProvider 隔离具体 OIDC 库与认证应用服务。
type OIDCProvider interface {
	// AuthorizationURL 创建包含 state、nonce 和 S256 challenge 的跳转地址。
	AuthorizationURL(state, nonce, challenge string) (string, error)
	// Exchange 使用一次性 code 和 verifier 换取并校验 ID token。
	Exchange(context.Context, string, string) (OIDCIdentity, error)
	// ProviderLogoutURL 返回启动时验证并构造的前台退出地址。
	ProviderLogoutURL() string
}

// MFAPolicy 保存只有管理账号需要满足的 provider 认证证据约束。
type MFAPolicy struct {
	// AllowedACR 非空时要求 token 的 acr 属于该集合。
	AllowedACR []string
	// RequiredAMR 非空时要求 token 的 amr 包含全部配置值。
	RequiredAMR []string
}

// RequestMetadata 保存可进入脱敏审计的受控请求信息。
type RequestMetadata struct {
	// RequestID 是 HTTP adapter 规范化后的请求关联标识。
	RequestID string
	// Source 是只用于不可逆摘要的来源材料，不会直接进入数据库。
	Source string
}

// LoginStart 是发起 OIDC 登录后需要由 HTTP adapter 返回浏览器的数据。
type LoginStart struct {
	// AuthorizationURL 是经过 provider adapter 构造的授权端点地址。
	AuthorizationURL string
	// BrowserCookie 是绑定 callback 浏览器的临时不透明值。
	BrowserCookie string
	// ExpiresAt 是临时 Cookie 和数据库流程的共同过期边界。
	ExpiresAt time.Time
}

// CallbackInput 保存 callback 中受大小限制后传入领域层的必要值。
type CallbackInput struct {
	// State 是 provider 原样返回的一次性关联值。
	State string
	// BrowserCookie 是登录发起时设置的临时浏览器关联值。
	BrowserCookie string
	// Code 是只能使用一次的 authorization code。
	Code string
	// Metadata 用于记录不含凭据的登录审计。
	Metadata RequestMetadata
}

// CallbackResult 保存成功创建本地会话后的浏览器输出。
type CallbackResult struct {
	// SessionToken 是只写入安全 Cookie 的不透明随机值。
	SessionToken string
	// ExpiresAt 是当前会话不可超过的绝对过期时间。
	ExpiresAt time.Time
	// RedirectTo 是按本地账号类型复核后的站内落点。
	RedirectTo string
}

// SessionView 保存当前已认证会话和由原始 Cookie 派生的 CSRF nonce。
type SessionView struct {
	// Session 是每次从持久化层重新授权后的会话摘要。
	Session Session
	// CSRFToken 是可返回浏览器但不能替代 Cookie 身份的会话绑定 nonce。
	CSRFToken string
}

// LogoutResult 表示本地撤销提交后浏览器必须继续的受信任退出导航。
type LogoutResult struct {
	// ProviderLogoutURL 是不含用户凭据且不可由请求输入修改的 IdP 前台退出地址。
	ProviderLogoutURL string
}

// Service 编排一次性 OIDC 流程、本地主体授权、服务端会话和安全审计。
type Service struct {
	repository        Repository
	provider          OIDCProvider
	providerLogoutURL string
	hasher            *SecretHasher
	pkce              *PKCECipher
	mfaPolicy         MFAPolicy
	now               func() time.Time
	newToken          func() (string, error)
	newID             func() uuid.UUID
}

// NewService 创建认证应用服务；所有安全依赖必须显式注入且管理员 MFA 策略不能为空。
func NewService(
	repository Repository,
	provider OIDCProvider,
	hasher *SecretHasher,
	pkce *PKCECipher,
	mfaPolicy MFAPolicy,
) (*Service, error) {
	if repository == nil || provider == nil || hasher == nil || pkce == nil {
		return nil, errors.New("认证服务依赖不能为空")
	}
	providerLogoutURL := provider.ProviderLogoutURL()
	if providerLogoutURL == "" {
		return nil, errors.New("OIDC provider 前台退出地址不能为空")
	}
	if len(mfaPolicy.AllowedACR) == 0 && len(mfaPolicy.RequiredAMR) == 0 {
		return nil, errors.New("管理员 MFA 策略不能为空")
	}
	return &Service{
		repository:        repository,
		provider:          provider,
		providerLogoutURL: providerLogoutURL,
		hasher:            hasher,
		pkce:              pkce,
		mfaPolicy:         mfaPolicy,
		now:               func() time.Time { return time.Now().UTC() },
		newToken:          NewOpaqueToken,
		newID:             uuid.New,
	}, nil
}

// StartLogin 创建最长十分钟且与当前浏览器绑定的一次性 OIDC 登录流程。
func (service *Service) StartLogin(
	ctx context.Context,
	returnTo string,
	metadata RequestMetadata,
) (LoginStart, error) {
	if metadata.RequestID == "" {
		return LoginStart{}, NewError(ErrorCodeInvalidRequest)
	}
	if returnTo == "" {
		returnTo = defaultMailboxLanding
	}
	normalizedReturnTo, err := NormalizeReturnPath(returnTo)
	if err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, err)
	}
	state, browserCookie, nonce, verifier, err := service.newFlowSecrets()
	if err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, err)
	}
	challenge, err := PKCEChallenge(verifier)
	if err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, err)
	}
	ciphertext, keyID, err := service.pkce.Encrypt(verifier)
	if err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, err)
	}
	now := service.now()
	expiresAt := now.Add(DefaultAuthFlowLifetime)
	flow := AuthFlowToCreate{
		ID:                     service.newID(),
		StateDigest:            service.hasher.Digest(SecretPurposeAuthFlowState, state),
		BrowserCookieDigest:    service.hasher.Digest(SecretPurposeAuthFlowCookie, browserCookie),
		NonceDigest:            service.hasher.Digest(SecretPurposeOIDCNonce, nonce),
		PKCEVerifierCiphertext: ciphertext,
		EncryptionKeyID:        keyID,
		ReturnTo:               normalizedReturnTo,
		CreatedAt:              now,
		ExpiresAt:              expiresAt,
	}
	if err = service.repository.CreateAuthFlow(ctx, flow); err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, err)
	}
	authorizationURL, err := service.provider.AuthorizationURL(state, nonce, challenge)
	if err != nil {
		return LoginStart{}, service.loginFailure(ctx, metadata, normalizeProviderError(err))
	}
	return LoginStart{
		AuthorizationURL: authorizationURL,
		BrowserCookie:    browserCookie,
		ExpiresAt:        expiresAt,
	}, nil
}

// CompleteLogin 原子消费登录流程，校验 OIDC 与本地授权，并建立新的服务端会话。
func (service *Service) CompleteLogin(ctx context.Context, input CallbackInput) (CallbackResult, error) {
	if input.State == "" || input.BrowserCookie == "" || input.Code == "" || input.Metadata.RequestID == "" {
		return CallbackResult{}, NewError(ErrorCodeAuthFlowInvalid)
	}
	now := service.now()
	flow, err := service.repository.ConsumeAuthFlow(
		ctx,
		service.hasher.Digest(SecretPurposeAuthFlowState, input.State),
		service.hasher.Digest(SecretPurposeAuthFlowCookie, input.BrowserCookie),
		now,
	)
	if err != nil {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, err)
	}
	verifier, err := service.pkce.Decrypt(flow.PKCEVerifierCiphertext, flow.EncryptionKeyID)
	if err != nil {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, err)
	}
	externalIdentity, err := service.provider.Exchange(ctx, input.Code, verifier)
	if err != nil {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, normalizeProviderError(err))
	}
	actualNonceDigest := service.hasher.Digest(SecretPurposeOIDCNonce, externalIdentity.Nonce)
	if !hmac.Equal(actualNonceDigest[:], flow.NonceDigest[:]) || externalIdentity.Issuer == "" || externalIdentity.Subject == "" {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, NewError(ErrorCodeAuthFlowInvalid))
	}
	principal, err := service.repository.ResolvePrincipal(ctx, externalIdentity.Issuer, externalIdentity.Subject)
	if err != nil {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, err)
	}
	if principal.AccountType == AccountTypeAdministrator && !service.mfaSatisfied(externalIdentity) {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, NewError(ErrorCodeAuthForbidden))
	}

	sessionToken, err := service.newToken()
	if err != nil {
		return CallbackResult{}, service.loginFailure(ctx, input.Metadata, NewError(ErrorCodePersistenceUnavailable))
	}
	csrfToken := service.hasher.DeriveCSRFToken(sessionToken)
	session := SessionToCreate{
		ID:                service.newID(),
		SessionDigest:     service.hasher.Digest(SecretPurposeSessionCookie, sessionToken),
		PrincipalID:       principal.ID,
		OIDCIssuer:        externalIdentity.Issuer,
		OIDCSubject:       externalIdentity.Subject,
		OIDCSessionID:     externalIdentity.SessionID,
		CSRFDigest:        service.hasher.Digest(SecretPurposeCSRFToken, csrfToken),
		CreatedAt:         now,
		IdleExpiresAt:     now.Add(DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: now.Add(DefaultSessionAbsoluteTimeout),
	}
	audit := service.auditEvent(input.Metadata, &principal.ID, "login", "succeeded", "", now)
	created, err := service.repository.CreateSessionWithAudit(ctx, session, audit)
	if err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{
		SessionToken: sessionToken,
		ExpiresAt:    created.AbsoluteExpiresAt,
		RedirectTo:   landingFor(principal.AccountType, flow.ReturnTo),
	}, nil
}

// RejectLogin 消费 provider 明确拒绝或用户取消的 callback 流程，并记录通用失败审计。
func (service *Service) RejectLogin(
	ctx context.Context,
	state string,
	browserCookie string,
	metadata RequestMetadata,
) error {
	if state == "" || browserCookie == "" || metadata.RequestID == "" {
		if metadata.RequestID == "" {
			return NewError(ErrorCodeAuthFlowInvalid)
		}
		return service.loginFailure(ctx, metadata, NewError(ErrorCodeAuthFlowInvalid))
	}
	_, err := service.repository.ConsumeAuthFlow(
		ctx,
		service.hasher.Digest(SecretPurposeAuthFlowState, state),
		service.hasher.Digest(SecretPurposeAuthFlowCookie, browserCookie),
		service.now(),
	)
	if err != nil {
		return service.loginFailure(ctx, metadata, err)
	}
	return service.loginFailure(ctx, metadata, NewError(ErrorCodeAuthFlowInvalid))
}

// ResolveSession 返回当前有效会话；缺少或失效 Cookie 统一视为匿名。
func (service *Service) ResolveSession(ctx context.Context, sessionToken string) (*SessionView, error) {
	if sessionToken == "" {
		return nil, nil
	}
	session, err := service.repository.ResolveSession(
		ctx,
		service.hasher.Digest(SecretPurposeSessionCookie, sessionToken),
		service.now(),
	)
	if HasErrorCode(err, ErrorCodeAuthRequired) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	csrfToken := service.hasher.DeriveCSRFToken(sessionToken)
	csrfDigest := service.hasher.Digest(SecretPurposeCSRFToken, csrfToken)
	if !hmac.Equal(csrfDigest[:], session.CSRFDigest[:]) {
		return nil, nil
	}
	return &SessionView{Session: session, CSRFToken: csrfToken}, nil
}

// Logout 校验当前有效会话的 CSRF nonce，并在同一事务中完成撤销和审计。
func (service *Service) Logout(
	ctx context.Context,
	sessionToken string,
	csrfToken string,
	metadata RequestMetadata,
) (LogoutResult, error) {
	result := LogoutResult{ProviderLogoutURL: service.providerLogoutURL}
	if sessionToken == "" {
		return result, nil
	}
	view, err := service.ResolveSession(ctx, sessionToken)
	if err != nil {
		return LogoutResult{}, err
	}
	if view == nil {
		return result, nil
	}
	providedDigest := service.hasher.Digest(SecretPurposeCSRFToken, csrfToken)
	if csrfToken == "" || !hmac.Equal(providedDigest[:], view.Session.CSRFDigest[:]) {
		return LogoutResult{}, NewError(ErrorCodeCSRFInvalid)
	}
	now := service.now()
	audit := service.auditEvent(metadata, &view.Session.Principal.ID, "logout", "succeeded", "", now)
	err = service.repository.RevokeSessionWithAudit(
		ctx,
		service.hasher.Digest(SecretPurposeSessionCookie, sessionToken),
		now,
		RevocationReasonLogout,
		audit,
	)
	if err != nil {
		return LogoutResult{}, err
	}
	return result, nil
}

// newFlowSecrets 生成互不复用的 state、浏览器关联值、nonce 和 PKCE verifier。
func (service *Service) newFlowSecrets() (string, string, string, string, error) {
	values := make([]string, 4)
	for index := range values {
		value, err := service.newToken()
		if err != nil {
			return "", "", "", "", NewError(ErrorCodePersistenceUnavailable)
		}
		values[index] = value
	}
	return values[0], values[1], values[2], values[3], nil
}

// mfaSatisfied 要求所有已配置的 ACR 与 AMR 策略同时满足。
func (service *Service) mfaSatisfied(externalIdentity OIDCIdentity) bool {
	if len(service.mfaPolicy.AllowedACR) > 0 && !slices.Contains(service.mfaPolicy.AllowedACR, externalIdentity.ACR) {
		return false
	}
	for _, required := range service.mfaPolicy.RequiredAMR {
		if !slices.Contains(externalIdentity.AMR, required) {
			return false
		}
	}
	return true
}

// loginFailure 尽力写入脱敏失败审计；审计不可确认时提升为服务不可用。
func (service *Service) loginFailure(ctx context.Context, metadata RequestMetadata, cause error) error {
	event := service.auditEvent(metadata, nil, "login", "failed", failureCategory(cause), service.now())
	if auditErr := service.repository.RecordAudit(ctx, event); auditErr != nil {
		return auditErr
	}
	return cause
}

// auditEvent 构造只包含稳定类别和来源摘要的认证审计事件。
func (service *Service) auditEvent(
	metadata RequestMetadata,
	principalID *uuid.UUID,
	action string,
	result string,
	failure string,
	occurredAt time.Time,
) AuditEvent {
	return AuditEvent{
		ID:              service.newID(),
		PrincipalID:     principalID,
		Action:          action,
		Result:          result,
		FailureCategory: failure,
		SourceDigest:    service.hasher.Digest(SecretPurposeSource, metadata.Source),
		RequestID:       metadata.RequestID,
		OccurredAt:      occurredAt,
	}
}

// failureCategory 把公开错误码压缩为不暴露主体存在性的稳定审计类别。
func failureCategory(err error) string {
	switch {
	case HasErrorCode(err, ErrorCodeAuthForbidden):
		return "access_denied"
	case HasErrorCode(err, ErrorCodePersistenceUnavailable):
		return "provider_unavailable"
	default:
		return "invalid_flow"
	}
}

// normalizeProviderError 防止具体 OIDC 库或网络错误越过领域边界。
func normalizeProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var identityError *Error
	if errors.As(err, &identityError) {
		return err
	}
	return NewError(ErrorCodePersistenceUnavailable)
}

// landingFor 只保留与本地账号类型一致的候选路径，否则进入固定默认落点。
func landingFor(accountType AccountType, candidate string) string {
	pathOnly := candidate
	if queryIndex := strings.IndexByte(candidate, '?'); queryIndex >= 0 {
		pathOnly = candidate[:queryIndex]
	}
	if accountType == AccountTypeMailbox {
		if pathOnly == "/mail" || strings.HasPrefix(pathOnly, "/mail/") {
			return candidate
		}
		return defaultMailboxLanding
	}
	if accountType == AccountTypeAdministrator {
		if pathOnly == "/admin" || strings.HasPrefix(pathOnly, "/admin/") {
			return candidate
		}
		return defaultAdminLanding
	}
	return "/forbidden"
}
