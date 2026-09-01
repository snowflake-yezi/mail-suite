package identity

import (
	"bytes"
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeOIDCProvider 让测试控制授权 URL 输入和 callback 身份证据。
type fakeOIDCProvider struct {
	identity  OIDCIdentity
	exchange  error
	state     string
	nonce     string
	challenge string
}

// AuthorizationURL 记录一次性参数并返回固定假授权端点。
func (provider *fakeOIDCProvider) AuthorizationURL(state, nonce, challenge string) (string, error) {
	provider.state = state
	provider.nonce = nonce
	provider.challenge = challenge
	return "https://idp.example.test/authorize?state=" + url.QueryEscape(state), nil
}

// Exchange 返回测试预设的已验证 OIDC 身份。
func (provider *fakeOIDCProvider) Exchange(context.Context, string, string) (OIDCIdentity, error) {
	return provider.identity, provider.exchange
}

// fakeIdentityRepository 记录领域服务是否使用预期的持久化原子边界。
type fakeIdentityRepository struct {
	createdFlow      AuthFlowToCreate
	consumedFlow     ConsumedAuthFlow
	consumeErr       error
	principal        Principal
	principalErr     error
	createdSession   SessionToCreate
	createAudit      AuditEvent
	resolvedSession  Session
	resolveErr       error
	revokedDigest    [32]byte
	revokeAudit      AuditEvent
	recordedAudits   []AuditEvent
	atomicCreateCall int
	atomicRevokeCall int
}

// CreateAuthFlow 保存测试流程输入。
func (repository *fakeIdentityRepository) CreateAuthFlow(_ context.Context, flow AuthFlowToCreate) error {
	repository.createdFlow = flow
	return nil
}

// ConsumeAuthFlow 返回预设的一次性流程。
func (repository *fakeIdentityRepository) ConsumeAuthFlow(
	context.Context,
	[32]byte,
	[32]byte,
	time.Time,
) (ConsumedAuthFlow, error) {
	return repository.consumedFlow, repository.consumeErr
}

// ResolvePrincipal 返回预设的本地主体。
func (repository *fakeIdentityRepository) ResolvePrincipal(context.Context, string, string) (Principal, error) {
	return repository.principal, repository.principalErr
}

// CreateSession 只用于满足 repository 接口，流程服务不应调用非原子版本。
func (repository *fakeIdentityRepository) CreateSession(context.Context, SessionToCreate) (CreatedSession, error) {
	return CreatedSession{}, nil
}

// CreateSessionWithAudit 记录会话与登录审计的原子调用。
func (repository *fakeIdentityRepository) CreateSessionWithAudit(
	_ context.Context,
	session SessionToCreate,
	event AuditEvent,
) (CreatedSession, error) {
	repository.atomicCreateCall++
	repository.createdSession = session
	repository.createAudit = event
	return CreatedSession{
		ID:                session.ID,
		PrincipalID:       session.PrincipalID,
		CreatedAt:         session.CreatedAt,
		IdleExpiresAt:     session.IdleExpiresAt,
		AbsoluteExpiresAt: session.AbsoluteExpiresAt,
	}, nil
}

// ResolveSession 返回预设的当前会话。
func (repository *fakeIdentityRepository) ResolveSession(context.Context, [32]byte, time.Time) (Session, error) {
	return repository.resolvedSession, repository.resolveErr
}

// RevokeSession 只用于满足 repository 接口，流程服务不应调用非原子版本。
func (repository *fakeIdentityRepository) RevokeSession(context.Context, [32]byte, time.Time, RevocationReason) error {
	return nil
}

// RevokeSessionWithAudit 记录退出撤销与审计的原子调用。
func (repository *fakeIdentityRepository) RevokeSessionWithAudit(
	_ context.Context,
	digest [32]byte,
	_ time.Time,
	_ RevocationReason,
	event AuditEvent,
) error {
	repository.atomicRevokeCall++
	repository.revokedDigest = digest
	repository.revokeAudit = event
	return nil
}

// RevokeByOIDCSessionID 满足后续 backchannel 使用的 repository 接口。
func (repository *fakeIdentityRepository) RevokeByOIDCSessionID(context.Context, string, string, time.Time) (int64, error) {
	return 0, nil
}

// RevokeByOIDCSubject 满足后续 backchannel 使用的 repository 接口。
func (repository *fakeIdentityRepository) RevokeByOIDCSubject(context.Context, string, string, time.Time) (int64, error) {
	return 0, nil
}

// RecordAudit 保存失败审计供断言。
func (repository *fakeIdentityRepository) RecordAudit(_ context.Context, event AuditEvent) error {
	repository.recordedAudits = append(repository.recordedAudits, event)
	return nil
}

func TestStartLoginPersistsOnlyDigestsAndEncryptedVerifier(t *testing.T) {
	repository := &fakeIdentityRepository{}
	provider := &fakeOIDCProvider{}
	service, hasher, cipherBox := newTestIdentityService(t, repository, provider)
	values := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"ccccccccccccccccccccccccccccccccccccccccccc",
		rfc7636Verifier,
	}
	service.newToken = func() (string, error) {
		value := values[0]
		values = values[1:]
		return value, nil
	}

	start, err := service.StartLogin(
		context.Background(),
		"/admin/overview?tab=domains",
		RequestMetadata{RequestID: "request-start"},
	)
	if err != nil {
		t.Fatalf("发起登录失败：%v", err)
	}
	if start.BrowserCookie != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || provider.state != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("state 与浏览器 Cookie 未保持独立：start=%+v state=%q", start, provider.state)
	}
	if repository.createdFlow.StateDigest != hasher.Digest(SecretPurposeAuthFlowState, provider.state) ||
		repository.createdFlow.NonceDigest != hasher.Digest(SecretPurposeOIDCNonce, provider.nonce) {
		t.Fatal("一次性 state 或 nonce 未以独立 purpose 摘要持久化")
	}
	verifier, err := cipherBox.Decrypt(repository.createdFlow.PKCEVerifierCiphertext, repository.createdFlow.EncryptionKeyID)
	if err != nil || verifier != rfc7636Verifier {
		t.Fatalf("持久化 PKCE verifier 密文无法恢复：verifier=%q err=%v", verifier, err)
	}
	if provider.challenge != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("授权请求未使用预期 S256 challenge：%q", provider.challenge)
	}
}

func TestStartLoginAuditsInvalidReturnPath(t *testing.T) {
	repository := &fakeIdentityRepository{}
	provider := &fakeOIDCProvider{}
	service, _, _ := newTestIdentityService(t, repository, provider)
	_, err := service.StartLogin(
		context.Background(),
		"https://evil.example.test/mail",
		RequestMetadata{RequestID: "request-invalid-return"},
	)
	if !HasErrorCode(err, ErrorCodeInvalidReturnTo) || len(repository.recordedAudits) != 1 {
		t.Fatalf("非法 return_to 应拒绝并审计：err=%v audits=%d", err, len(repository.recordedAudits))
	}
	if repository.recordedAudits[0].FailureCategory != "invalid_flow" || repository.createdFlow.ID != uuid.Nil {
		t.Fatalf("非法 return_to 审计或持久化边界错误：audit=%+v flow=%+v", repository.recordedAudits[0], repository.createdFlow)
	}
}

func TestCompleteLoginCreatesAuditedMailboxSessionAndRestrictsReturnPath(t *testing.T) {
	repository := &fakeIdentityRepository{}
	provider := &fakeOIDCProvider{}
	service, hasher, cipherBox := newTestIdentityService(t, repository, provider)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.newToken = func() (string, error) { return "sssssssssssssssssssssssssssssssssssssssssss", nil }
	verifierCiphertext, keyID, err := cipherBox.Encrypt(rfc7636Verifier)
	if err != nil {
		t.Fatalf("准备 verifier 密文失败：%v", err)
	}
	repository.consumedFlow = ConsumedAuthFlow{
		NonceDigest:            hasher.Digest(SecretPurposeOIDCNonce, "expected-nonce"),
		PKCEVerifierCiphertext: verifierCiphertext,
		EncryptionKeyID:        keyID,
		ReturnTo:               "/admin/overview",
	}
	repository.principal = Principal{
		ID:          uuid.New(),
		TenantID:    uuid.New(),
		AccountType: AccountTypeMailbox,
		DisplayName: "Alice",
		Mailbox:     &Mailbox{ID: uuid.New(), Address: "alice@example.test"},
	}
	provider.identity = OIDCIdentity{
		Issuer:  "https://idp.example.test/realms/mail-suite",
		Subject: "alice-subject",
		Nonce:   "expected-nonce",
	}

	result, err := service.CompleteLogin(context.Background(), CallbackInput{
		State:         "state",
		BrowserCookie: "browser",
		Code:          "code",
		Metadata:      RequestMetadata{RequestID: "request-1", Source: "source"},
	})
	if err != nil {
		t.Fatalf("完成邮箱登录失败：%v", err)
	}
	if result.RedirectTo != defaultMailboxLanding || repository.atomicCreateCall != 1 {
		t.Fatalf("跨端 return_to 应收敛且会话必须原子创建：result=%+v calls=%d", result, repository.atomicCreateCall)
	}
	if repository.createAudit.Action != "login" || repository.createAudit.Result != "succeeded" || repository.createAudit.PrincipalID == nil {
		t.Fatalf("登录成功审计不完整：%+v", repository.createAudit)
	}
	if repository.createdSession.CSRFDigest != hasher.Digest(SecretPurposeCSRFToken, hasher.DeriveCSRFToken(result.SessionToken)) {
		t.Fatal("新会话未绑定由 Cookie 派生的 CSRF nonce")
	}
}

func TestCompleteLoginRejectsNonceMismatchAndAdministratorWithoutMFA(t *testing.T) {
	tests := []struct {
		name      string
		principal Principal
		identity  OIDCIdentity
	}{
		{
			name:      "nonce mismatch",
			principal: Principal{ID: uuid.New(), AccountType: AccountTypeMailbox},
			identity:  OIDCIdentity{Issuer: "https://idp.example.test", Subject: "user", Nonce: "wrong"},
		},
		{
			name:      "administrator without MFA",
			principal: Principal{ID: uuid.New(), AccountType: AccountTypeAdministrator},
			identity:  OIDCIdentity{Issuer: "https://idp.example.test", Subject: "admin", Nonce: "expected", ACR: "password"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeIdentityRepository{principal: test.principal}
			provider := &fakeOIDCProvider{identity: test.identity}
			service, hasher, cipherBox := newTestIdentityService(t, repository, provider)
			ciphertext, keyID, err := cipherBox.Encrypt(rfc7636Verifier)
			if err != nil {
				t.Fatalf("准备 verifier 密文失败：%v", err)
			}
			repository.consumedFlow = ConsumedAuthFlow{
				NonceDigest:            hasher.Digest(SecretPurposeOIDCNonce, "expected"),
				PKCEVerifierCiphertext: ciphertext,
				EncryptionKeyID:        keyID,
				ReturnTo:               "/admin/overview",
			}
			_, err = service.CompleteLogin(context.Background(), CallbackInput{
				State: "state", BrowserCookie: "browser", Code: "code",
				Metadata: RequestMetadata{RequestID: "request-2"},
			})
			if err == nil || repository.atomicCreateCall != 0 || len(repository.recordedAudits) != 1 {
				t.Fatalf("认证证据不足不得创建会话且必须审计：err=%v creates=%d audits=%d", err, repository.atomicCreateCall, len(repository.recordedAudits))
			}
		})
	}
}

func TestResolveSessionAndLogoutRequireBoundCSRFToken(t *testing.T) {
	repository := &fakeIdentityRepository{}
	provider := &fakeOIDCProvider{}
	service, hasher, _ := newTestIdentityService(t, repository, provider)
	const sessionToken = "session-cookie-value"
	csrfToken := hasher.DeriveCSRFToken(sessionToken)
	repository.resolvedSession = Session{
		ID: uuid.New(),
		Principal: Principal{
			ID: uuid.New(), TenantID: uuid.New(), AccountType: AccountTypeAdministrator,
		},
		CSRFDigest:        hasher.Digest(SecretPurposeCSRFToken, csrfToken),
		IdleExpiresAt:     time.Now().Add(time.Minute),
		AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}
	view, err := service.ResolveSession(context.Background(), sessionToken)
	if err != nil || view == nil || view.CSRFToken != csrfToken {
		t.Fatalf("读取有效会话失败：view=%+v err=%v", view, err)
	}
	repository.resolvedSession.CSRFDigest = [32]byte{0xff}
	invalidView, err := service.ResolveSession(context.Background(), sessionToken)
	if err != nil || invalidView != nil {
		t.Fatalf("CSRF 摘要损坏的旧会话应按匿名处理：view=%+v err=%v", invalidView, err)
	}
	repository.resolvedSession.CSRFDigest = hasher.Digest(SecretPurposeCSRFToken, csrfToken)
	if err = service.Logout(context.Background(), sessionToken, "wrong", RequestMetadata{RequestID: "request-3"}); !HasErrorCode(err, ErrorCodeCSRFInvalid) {
		t.Fatalf("错误 CSRF token 必须拒绝退出，实际为 %v", err)
	}
	if err = service.Logout(context.Background(), sessionToken, csrfToken, RequestMetadata{RequestID: "request-4"}); err != nil {
		t.Fatalf("有效 CSRF token 退出失败：%v", err)
	}
	if repository.atomicRevokeCall != 1 || repository.revokeAudit.Action != "logout" {
		t.Fatalf("退出未使用原子撤销与审计：calls=%d audit=%+v", repository.atomicRevokeCall, repository.revokeAudit)
	}
}

// newTestIdentityService 创建只使用假凭据和固定 MFA 策略的领域服务。
func newTestIdentityService(
	t *testing.T,
	repository Repository,
	provider OIDCProvider,
) (*Service, *SecretHasher, *PKCECipher) {
	t.Helper()
	hasher, err := NewSecretHasher(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatalf("创建测试摘要器失败：%v", err)
	}
	cipherBox, err := NewPKCECipher("test-v1", map[string][]byte{
		"test-v1": bytes.Repeat([]byte{0x72}, 32),
	})
	if err != nil {
		t.Fatalf("创建测试 PKCE cipher 失败：%v", err)
	}
	service, err := NewService(repository, provider, hasher, cipherBox, MFAPolicy{
		AllowedACR: []string{"urn:example:mfa"},
	})
	if err != nil {
		t.Fatalf("创建认证领域服务失败：%v", err)
	}
	return service, hasher, cipherBox
}
