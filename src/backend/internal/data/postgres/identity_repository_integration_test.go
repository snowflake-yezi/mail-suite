package postgres

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

func TestIdentityRepositoryEnforcesOneTimeFlowAndCurrentSessionAuthorization(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过身份会话真实 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("应用身份认证 migration 失败：%v", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建身份认证测试连接池失败：%v", err)
	}
	t.Cleanup(pool.Close)

	fixtures := insertIdentityFixtures(t, ctx, pool)
	t.Cleanup(func() { deleteIdentityFixtures(t, ctx, pool, fixtures) })
	repository, err := NewIdentityRepository(
		pool,
		identity.DefaultSessionIdleTimeout,
		identity.DefaultSessionTouchInterval,
	)
	if err != nil {
		t.Fatalf("创建身份 repository 失败：%v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	stateDigest := testDigest("state-" + fixtures.suffix)
	browserDigest := testDigest("browser-" + fixtures.suffix)
	nonceDigest := testDigest("nonce-" + fixtures.suffix)
	flowID := uuid.New()
	fixtures.flowID = flowID
	err = repository.CreateAuthFlow(ctx, identity.AuthFlowToCreate{
		ID:                     flowID,
		StateDigest:            stateDigest,
		BrowserCookieDigest:    browserDigest,
		NonceDigest:            nonceDigest,
		PKCEVerifierCiphertext: make([]byte, 64),
		EncryptionKeyID:        "test-key-v1",
		ReturnTo:               "/mail/inbox",
		CreatedAt:              now,
		ExpiresAt:              now.Add(identity.DefaultAuthFlowLifetime),
	})
	if err != nil {
		t.Fatalf("创建一次性登录流程失败：%v", err)
	}
	_, err = repository.ConsumeAuthFlow(ctx, stateDigest, testDigest("other-browser"), now.Add(time.Second))
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthFlowInvalid) {
		t.Fatalf("浏览器关联不匹配应隐藏为登录流程失效，实际为 %v", err)
	}
	consumed, err := repository.ConsumeAuthFlow(ctx, stateDigest, browserDigest, now.Add(2*time.Second))
	if err != nil || consumed.ID != flowID || consumed.NonceDigest != nonceDigest {
		t.Fatalf("原子消费登录流程失败：flow=%+v err=%v", consumed, err)
	}
	_, err = repository.ConsumeAuthFlow(ctx, stateDigest, browserDigest, now.Add(3*time.Second))
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthFlowInvalid) {
		t.Fatalf("重复 callback 必须被拒绝，实际为 %v", err)
	}

	mailboxPrincipal, err := repository.ResolvePrincipal(ctx, fixtures.issuer, fixtures.mailboxSubject)
	if err != nil {
		t.Fatalf("解析邮箱主体失败：%v", err)
	}
	if mailboxPrincipal.AccountType != identity.AccountTypeMailbox ||
		mailboxPrincipal.Mailbox == nil ||
		mailboxPrincipal.Mailbox.ID != fixtures.mailboxID ||
		mailboxPrincipal.Mailbox.Address != fixtures.mailboxAddress {
		t.Fatalf("邮箱主体映射错误：%+v", mailboxPrincipal)
	}
	adminPrincipal, err := repository.ResolvePrincipal(ctx, fixtures.issuer, fixtures.adminSubject)
	if err != nil || adminPrincipal.AccountType != identity.AccountTypeAdministrator || adminPrincipal.Mailbox != nil {
		t.Fatalf("管理主体映射错误：principal=%+v err=%v", adminPrincipal, err)
	}
	_, err = repository.ResolvePrincipal(ctx, fixtures.issuer, fixtures.unprivilegedAdminSubject)
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthForbidden) {
		t.Fatalf("缺少管理入口权限必须使用通用拒绝，实际为 %v", err)
	}
	_, err = repository.CreateSession(ctx, identity.SessionToCreate{
		ID:                uuid.New(),
		SessionDigest:     testDigest("mismatched-identity-" + fixtures.suffix),
		PrincipalID:       fixtures.mailboxPrincipalID,
		OIDCIssuer:        fixtures.issuer,
		OIDCSubject:       fixtures.adminSubject,
		CSRFDigest:        testDigest("mismatched-csrf-" + fixtures.suffix),
		CreatedAt:         now,
		IdleExpiresAt:     now.Add(identity.DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: now.Add(identity.DefaultSessionAbsoluteTimeout),
	})
	if !identity.HasErrorCode(err, identity.ErrorCodePersistenceUnavailable) {
		t.Fatalf("会话不得把已验证外部身份错绑到其他本地主体，实际为 %v", err)
	}

	createdAt := now.Add(-6 * time.Minute)
	mailboxSessionDigest := testDigest("mailbox-session-" + fixtures.suffix)
	csrfDigest := testDigest("csrf-" + fixtures.suffix)
	rolledBackSessionDigest := testDigest("rolled-back-session-" + fixtures.suffix)
	_, err = repository.CreateSessionWithAudit(ctx, identity.SessionToCreate{
		ID:                uuid.New(),
		SessionDigest:     rolledBackSessionDigest,
		PrincipalID:       fixtures.mailboxPrincipalID,
		OIDCIssuer:        fixtures.issuer,
		OIDCSubject:       fixtures.mailboxSubject,
		CSRFDigest:        testDigest("rolled-back-csrf-" + fixtures.suffix),
		CreatedAt:         now,
		IdleExpiresAt:     now.Add(identity.DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: now.Add(identity.DefaultSessionAbsoluteTimeout),
	}, identity.AuditEvent{
		ID:           uuid.New(),
		PrincipalID:  &fixtures.mailboxPrincipalID,
		Action:       "login",
		Result:       "succeeded",
		SourceDigest: testDigest("rolled-back-source-" + fixtures.suffix),
		RequestID:    strings.Repeat("x", 65),
		OccurredAt:   now,
	})
	if !identity.HasErrorCode(err, identity.ErrorCodePersistenceUnavailable) {
		t.Fatalf("审计写入失败必须回滚会话创建，实际为 %v", err)
	}
	var rolledBackSessionCount int
	if err = pool.QueryRow(
		ctx,
		"SELECT count(*) FROM user_sessions WHERE session_digest = $1",
		rolledBackSessionDigest[:],
	).Scan(&rolledBackSessionCount); err != nil || rolledBackSessionCount != 0 {
		t.Fatalf("审计失败后不得残留会话：count=%d err=%v", rolledBackSessionCount, err)
	}
	_, err = repository.CreateSessionWithAudit(ctx, identity.SessionToCreate{
		ID:                uuid.New(),
		SessionDigest:     mailboxSessionDigest,
		PrincipalID:       fixtures.mailboxPrincipalID,
		OIDCIssuer:        fixtures.issuer,
		OIDCSubject:       fixtures.mailboxSubject,
		OIDCSessionID:     "mailbox-sid",
		CSRFDigest:        csrfDigest,
		CreatedAt:         createdAt,
		IdleExpiresAt:     createdAt.Add(identity.DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: createdAt.Add(identity.DefaultSessionAbsoluteTimeout),
	}, identity.AuditEvent{
		ID:           uuid.New(),
		PrincipalID:  &fixtures.mailboxPrincipalID,
		Action:       "login",
		Result:       "succeeded",
		SourceDigest: testDigest("login-source-" + fixtures.suffix),
		RequestID:    "identity-test-" + fixtures.suffix,
		OccurredAt:   createdAt,
	})
	if err != nil {
		t.Fatalf("创建邮箱会话失败：%v", err)
	}
	resolved, err := repository.ResolveSession(ctx, mailboxSessionDigest, now)
	if err != nil {
		t.Fatalf("读取邮箱会话失败：%v", err)
	}
	if resolved.Principal.Mailbox == nil || resolved.CSRFDigest != csrfDigest {
		t.Fatalf("会话未返回固定邮箱或 CSRF 摘要：%+v", resolved)
	}
	if resolved.IdleExpiresAt.Before(now.Add(29*time.Minute)) ||
		resolved.IdleExpiresAt.After(now.Add(identity.DefaultSessionIdleTimeout)) {
		t.Fatalf("限频 touch 未按 30 分钟边界刷新空闲期限：%s", resolved.IdleExpiresAt)
	}

	if _, err = pool.Exec(
		ctx,
		"UPDATE identity_principals SET status = 'suspended', updated_at = $2 WHERE id = $1",
		fixtures.mailboxPrincipalID,
		now,
	); err != nil {
		t.Fatalf("停用邮箱主体失败：%v", err)
	}
	_, err = repository.ResolveSession(ctx, mailboxSessionDigest, now.Add(time.Second))
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthRequired) {
		t.Fatalf("主体停用后既有会话必须立即失效，实际为 %v", err)
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE identity_principals SET status = 'active', updated_at = $2 WHERE id = $1",
		fixtures.mailboxPrincipalID,
		now.Add(2*time.Second),
	); err != nil {
		t.Fatalf("恢复邮箱主体测试状态失败：%v", err)
	}
	err = repository.RevokeSessionWithAudit(
		ctx,
		mailboxSessionDigest,
		now.Add(2500*time.Millisecond),
		identity.RevocationReasonLogout,
		identity.AuditEvent{
			ID:           uuid.New(),
			PrincipalID:  &fixtures.mailboxPrincipalID,
			Action:       "logout",
			Result:       "succeeded",
			SourceDigest: testDigest("rolled-back-logout-source-" + fixtures.suffix),
			RequestID:    strings.Repeat("y", 65),
			OccurredAt:   now.Add(2500 * time.Millisecond),
		},
	)
	if !identity.HasErrorCode(err, identity.ErrorCodePersistenceUnavailable) {
		t.Fatalf("退出审计失败必须回滚会话撤销，实际为 %v", err)
	}
	if _, err = repository.ResolveSession(ctx, mailboxSessionDigest, now.Add(2750*time.Millisecond)); err != nil {
		t.Fatalf("退出审计失败后原会话必须仍有效：%v", err)
	}
	if err = repository.RevokeSessionWithAudit(
		ctx,
		mailboxSessionDigest,
		now.Add(3*time.Second),
		identity.RevocationReasonLogout,
		identity.AuditEvent{
			ID:           uuid.New(),
			PrincipalID:  &fixtures.mailboxPrincipalID,
			Action:       "logout",
			Result:       "succeeded",
			SourceDigest: testDigest("logout-source-" + fixtures.suffix),
			RequestID:    "identity-test-" + fixtures.suffix,
			OccurredAt:   now.Add(3 * time.Second),
		},
	); err != nil {
		t.Fatalf("撤销邮箱会话失败：%v", err)
	}
	if err = repository.RevokeSession(
		ctx,
		mailboxSessionDigest,
		now.Add(4*time.Second),
		identity.RevocationReasonLogout,
	); err != nil {
		t.Fatalf("重复撤销必须保持幂等：%v", err)
	}
	_, err = repository.ResolveSession(ctx, mailboxSessionDigest, now.Add(5*time.Second))
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthRequired) {
		t.Fatalf("已撤销会话不得恢复，实际为 %v", err)
	}

	firstAdminDigest := createIdentityTestSession(
		t,
		ctx,
		repository,
		fixtures,
		"admin-session-one",
		"admin-sid-one",
		now,
	)
	secondAdminDigest := createIdentityTestSession(
		t,
		ctx,
		repository,
		fixtures,
		"admin-session-two",
		"admin-sid-two",
		now,
	)
	revoked, err := repository.RevokeByOIDCSessionID(
		ctx,
		fixtures.issuer,
		"admin-sid-one",
		now.Add(time.Minute),
	)
	if err != nil || revoked != 1 {
		t.Fatalf("按 sid 精确撤销失败：count=%d err=%v", revoked, err)
	}
	if _, err = repository.ResolveSession(ctx, firstAdminDigest, now.Add(2*time.Minute)); !identity.HasErrorCode(err, identity.ErrorCodeAuthRequired) {
		t.Fatalf("sid 匹配会话应失效，实际为 %v", err)
	}
	if _, err = repository.ResolveSession(ctx, secondAdminDigest, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("不同 sid 的设备会话不应被误撤销：%v", err)
	}
	revoked, err = repository.RevokeByOIDCSubject(
		ctx,
		fixtures.issuer,
		fixtures.adminSubject,
		now.Add(3*time.Minute),
	)
	if err != nil || revoked != 1 {
		t.Fatalf("按 subject 撤销剩余设备会话失败：count=%d err=%v", revoked, err)
	}
	if _, err = repository.ResolveSession(ctx, secondAdminDigest, now.Add(4*time.Minute)); !identity.HasErrorCode(err, identity.ErrorCodeAuthRequired) {
		t.Fatalf("subject 匹配会话应全部失效，实际为 %v", err)
	}

	permissionDigest := createIdentityTestSession(
		t,
		ctx,
		repository,
		fixtures,
		"admin-permission-session",
		"admin-permission-sid",
		now,
	)
	if _, err = pool.Exec(
		ctx,
		"DELETE FROM principal_permissions WHERE principal_id = $1 AND permission_name = $2",
		fixtures.adminPrincipalID,
		identity.PermissionPortalAdminAccess,
	); err != nil {
		t.Fatalf("撤销管理入口权限失败：%v", err)
	}
	_, err = repository.ResolveSession(ctx, permissionDigest, now.Add(time.Second))
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthRequired) {
		t.Fatalf("管理入口权限撤销后既有会话必须失效，实际为 %v", err)
	}

	sourceDigest := testDigest("source-" + fixtures.suffix)
	err = repository.RecordAudit(ctx, identity.AuditEvent{
		ID:           uuid.New(),
		PrincipalID:  &fixtures.mailboxPrincipalID,
		Action:       "logout",
		Result:       "succeeded",
		SourceDigest: sourceDigest,
		RequestID:    "identity-test-" + fixtures.suffix,
		OccurredAt:   now,
	})
	if err != nil {
		t.Fatalf("写入脱敏认证审计失败：%v", err)
	}
	var auditCount int
	if err = pool.QueryRow(
		ctx,
		"SELECT count(*) FROM authentication_audit_events WHERE request_id = $1",
		"identity-test-"+fixtures.suffix,
	).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("认证审计行数错误：count=%d err=%v", auditCount, err)
	}
}

type identityTestFixtures struct {
	suffix                   string
	tenantID                 uuid.UUID
	otherTenantID            uuid.UUID
	domainID                 uuid.UUID
	mailboxID                uuid.UUID
	mailboxPrincipalID       uuid.UUID
	adminPrincipalID         uuid.UUID
	unprivilegedAdminID      uuid.UUID
	issuer                   string
	mailboxSubject           string
	adminSubject             string
	unprivilegedAdminSubject string
	mailboxAddress           string
	flowID                   uuid.UUID
}

func insertIdentityFixtures(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) identityTestFixtures {
	t.Helper()
	suffix := uuid.NewString()
	fixtures := identityTestFixtures{
		suffix:                   suffix,
		tenantID:                 uuid.New(),
		otherTenantID:            uuid.New(),
		domainID:                 uuid.New(),
		mailboxID:                uuid.New(),
		mailboxPrincipalID:       uuid.New(),
		adminPrincipalID:         uuid.New(),
		unprivilegedAdminID:      uuid.New(),
		issuer:                   "https://idp.example.test/realms/mail-suite-" + suffix,
		mailboxSubject:           "mailbox-" + suffix,
		adminSubject:             "administrator-" + suffix,
		unprivilegedAdminSubject: "administrator-no-access-" + suffix,
		mailboxAddress:           "alice@auth-" + suffix + ".example.test",
	}
	_, err := pool.Exec(
		ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2), ($3, $4)`,
		fixtures.tenantID,
		"认证测试租户 "+suffix,
		fixtures.otherTenantID,
		"认证测试其他租户 "+suffix,
	)
	if err != nil {
		t.Fatalf("写入认证测试租户失败：%v", err)
	}
	domainName := "auth-" + suffix + ".example.test"
	_, err = pool.Exec(
		ctx,
		`INSERT INTO domains (id, tenant_id, name) VALUES ($1, $2, $3)`,
		fixtures.domainID,
		fixtures.tenantID,
		domainName,
	)
	if err != nil {
		t.Fatalf("写入认证测试域名失败：%v", err)
	}
	_, err = pool.Exec(
		ctx,
		`INSERT INTO mailboxes (
            id, tenant_id, domain_id, local_part, display_name, desired_status, observed_status, revision
        ) VALUES ($1, $2, $3, 'alice', 'Alice', 'active', 'unknown', 1)`,
		fixtures.mailboxID,
		fixtures.tenantID,
		fixtures.domainID,
	)
	if err != nil {
		t.Fatalf("写入认证测试邮箱失败：%v", err)
	}
	_, err = pool.Exec(
		ctx,
		`INSERT INTO identity_principals (
            id, tenant_id, oidc_issuer, oidc_subject, account_type, mailbox_id, display_name
        ) VALUES
            ($1, $2, $3, $4, 'mailbox', $5, 'Alice'),
            ($6, $2, $3, $7, 'administrator', NULL, 'Admin'),
            ($8, $2, $3, $9, 'administrator', NULL, 'No Access')`,
		fixtures.mailboxPrincipalID,
		fixtures.tenantID,
		fixtures.issuer,
		fixtures.mailboxSubject,
		fixtures.mailboxID,
		fixtures.adminPrincipalID,
		fixtures.adminSubject,
		fixtures.unprivilegedAdminID,
		fixtures.unprivilegedAdminSubject,
	)
	if err != nil {
		t.Fatalf("写入认证测试主体失败：%v", err)
	}
	_, err = pool.Exec(
		ctx,
		`INSERT INTO principal_permissions (principal_id, permission_name) VALUES ($1, $2)`,
		fixtures.adminPrincipalID,
		identity.PermissionPortalAdminAccess,
	)
	if err != nil {
		t.Fatalf("写入认证测试管理权限失败：%v", err)
	}
	_, err = pool.Exec(
		ctx,
		`INSERT INTO identity_principals (
            id, tenant_id, oidc_issuer, oidc_subject, account_type, mailbox_id, display_name
        ) VALUES ($1, $2, $3, $4, 'mailbox', $5, 'Cross Tenant')`,
		uuid.New(),
		fixtures.otherTenantID,
		fixtures.issuer,
		"cross-tenant-"+suffix,
		fixtures.mailboxID,
	)
	if err == nil {
		t.Fatal("邮箱主体不得通过外键绑定其他租户邮箱")
	}
	return fixtures
}

func createIdentityTestSession(
	t *testing.T,
	ctx context.Context,
	repository *IdentityRepository,
	fixtures identityTestFixtures,
	digestLabel string,
	oidcSessionID string,
	createdAt time.Time,
) [32]byte {
	t.Helper()
	digest := testDigest(digestLabel + "-" + fixtures.suffix)
	_, err := repository.CreateSession(ctx, identity.SessionToCreate{
		ID:                uuid.New(),
		SessionDigest:     digest,
		PrincipalID:       fixtures.adminPrincipalID,
		OIDCIssuer:        fixtures.issuer,
		OIDCSubject:       fixtures.adminSubject,
		OIDCSessionID:     oidcSessionID,
		CSRFDigest:        testDigest("csrf-" + digestLabel + "-" + fixtures.suffix),
		CreatedAt:         createdAt,
		IdleExpiresAt:     createdAt.Add(identity.DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: createdAt.Add(identity.DefaultSessionAbsoluteTimeout),
	})
	if err != nil {
		t.Fatalf("创建管理会话失败：%v", err)
	}
	return digest
}

func deleteIdentityFixtures(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	fixtures identityTestFixtures,
) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM authentication_audit_events WHERE request_id = $1", []any{"identity-test-" + fixtures.suffix}},
		{"DELETE FROM user_sessions WHERE principal_id IN ($1, $2, $3)", []any{fixtures.mailboxPrincipalID, fixtures.adminPrincipalID, fixtures.unprivilegedAdminID}},
		{"DELETE FROM auth_flows WHERE id = $1", []any{fixtures.flowID}},
		{"DELETE FROM principal_permissions WHERE principal_id IN ($1, $2, $3)", []any{fixtures.mailboxPrincipalID, fixtures.adminPrincipalID, fixtures.unprivilegedAdminID}},
		{"DELETE FROM identity_principals WHERE id IN ($1, $2, $3)", []any{fixtures.mailboxPrincipalID, fixtures.adminPrincipalID, fixtures.unprivilegedAdminID}},
		{"DELETE FROM mailboxes WHERE id = $1", []any{fixtures.mailboxID}},
		{"DELETE FROM domains WHERE id = $1", []any{fixtures.domainID}},
		{"DELETE FROM tenants WHERE id IN ($1, $2)", []any{fixtures.tenantID, fixtures.otherTenantID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("清理身份认证测试数据失败：%v", err)
		}
	}
}

func testDigest(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}
