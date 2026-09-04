package testbootstrap

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

func TestApplyAndRemoveEnforceIdempotenceDriftAndOwnership(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过测试身份 bootstrap 真实 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("应用测试身份 bootstrap migration 失败：%v", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建测试身份 bootstrap 连接池失败：%v", err)
	}
	t.Cleanup(pool.Close)

	manifest := uniqueIntegrationManifest()
	t.Cleanup(func() { forceDeleteIntegrationFixture(t, ctx, pool, manifest) })
	type applyOutcome struct {
		result Result
		err    error
	}
	outcomes := make(chan applyOutcome, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, applyErr := Apply(ctx, pool, manifest)
			outcomes <- applyOutcome{result: result, err: applyErr}
		}()
	}
	waitGroup.Wait()
	close(outcomes)
	changedCount := 0
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("并发初始化测试身份 fixture 失败：%v", outcome.err)
		}
		if outcome.result.Changed {
			changedCount++
		}
	}
	if changedCount != 1 {
		t.Fatalf("两个并发初始化只能有一个创建数据：changed=%d", changedCount)
	}
	assertFixtureCounts(t, ctx, pool, manifest, 1, 1, 2, 4, 2)
	assertUnmappedSubjectAbsent(t, ctx, pool, manifest)

	replayed, err := Apply(ctx, pool, manifest)
	if err != nil || replayed.Changed {
		t.Fatalf("重复初始化必须无变化：result=%+v err=%v", replayed, err)
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE mailboxes SET observed_status = 'active' WHERE id = $1",
		manifest.Mailbox.MailboxID,
	); err != nil {
		t.Fatalf("推进测试邮箱 observed state 失败：%v", err)
	}
	if observedReplay, applyErr := Apply(ctx, pool, manifest); applyErr != nil || observedReplay.Changed {
		t.Fatalf("合法 observed state 演进不应被当作漂移：result=%+v err=%v", observedReplay, applyErr)
	}

	if _, err = pool.Exec(ctx, `
        UPDATE identity_principals SET display_name = 'Drifted'
        WHERE id = $1
    `, manifest.Administrator.PrincipalID); err != nil {
		t.Fatalf("制造主体漂移失败：%v", err)
	}
	if _, err = pool.Exec(ctx, `
        DELETE FROM principal_permissions WHERE principal_id = $1 AND permission_name = $2
    `, manifest.MFAInsufficient.PrincipalID, identity.PermissionPortalAdminAccess); err != nil {
		t.Fatalf("制造权限缺失失败：%v", err)
	}
	if _, err = Apply(ctx, pool, manifest); !errors.Is(err, ErrDrift) {
		t.Fatalf("字段漂移必须拒绝整笔初始化：%v", err)
	}
	var permissionCount int
	if err = pool.QueryRow(ctx, `
        SELECT count(*) FROM principal_permissions
        WHERE principal_id = $1 AND permission_name = $2
    `, manifest.MFAInsufficient.PrincipalID, identity.PermissionPortalAdminAccess).Scan(&permissionCount); err != nil {
		t.Fatalf("读取漂移后的权限失败：%v", err)
	}
	if permissionCount != 0 {
		t.Fatal("漂移失败必须回滚事务前半段补写的权限")
	}
	if _, err = pool.Exec(ctx, `
        UPDATE identity_principals SET display_name = $2 WHERE id = $1
    `,
		manifest.Administrator.PrincipalID,
		manifest.Administrator.DisplayName,
	); err != nil {
		t.Fatalf("恢复测试身份主体失败：%v", err)
	}
	if _, err = pool.Exec(ctx, `
        INSERT INTO principal_permissions (principal_id, permission_name) VALUES ($1, $2)
    `, manifest.MFAInsufficient.PrincipalID, identity.PermissionPortalAdminAccess); err != nil {
		t.Fatalf("恢复测试身份权限失败：%v", err)
	}

	extraDomainID := uuid.New()
	if _, err = pool.Exec(ctx, `
        INSERT INTO domains (id, tenant_id, name, status)
        VALUES ($1, $2, $3, 'active')
    `, extraDomainID, manifest.Tenant.ID, "extra-"+extraDomainID.String()+".test"); err != nil {
		t.Fatalf("创建回收边界外域名失败：%v", err)
	}
	if _, err = Remove(ctx, pool, manifest); !errors.Is(err, ErrUnsafeRemoval) {
		t.Fatalf("存在额外租户资源时必须拒绝回收：%v", err)
	}
	assertFixtureCounts(t, ctx, pool, manifest, 1, 1, 2, 4, 2)
	if _, err = pool.Exec(ctx, "DELETE FROM domains WHERE id = $1", extraDomainID); err != nil {
		t.Fatalf("清理回收边界外域名失败：%v", err)
	}

	insertSessionAndAudit(t, ctx, pool, manifest)
	removed, err := Remove(ctx, pool, manifest)
	if err != nil || !removed.Changed {
		t.Fatalf("显式回收测试身份 fixture 失败：result=%+v err=%v", removed, err)
	}
	assertFixtureCounts(t, ctx, pool, manifest, 0, 0, 0, 0, 0)
	repeatedRemoval, err := Remove(ctx, pool, manifest)
	if err != nil || repeatedRemoval.Changed {
		t.Fatalf("重复回收必须无变化：result=%+v err=%v", repeatedRemoval, err)
	}
}

func TestApplyRejectsDatabaseWithoutIdentityMigration(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过未迁移数据库真实 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建未迁移数据库管理连接池失败：%v", err)
	}
	t.Cleanup(adminPool.Close)

	databaseName := "bootstrap_missing_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedDatabaseName := pgx.Identifier{databaseName}.Sanitize()
	if _, err = adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabaseName); err != nil {
		t.Fatalf("创建未迁移测试数据库失败：%v", err)
	}
	t.Cleanup(func() {
		if _, dropErr := adminPool.Exec(
			context.Background(),
			"DROP DATABASE IF EXISTS "+quotedDatabaseName,
		); dropErr != nil {
			t.Errorf("清理未迁移测试数据库失败：%v", dropErr)
		}
	})

	targetConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析未迁移测试数据库连接失败：%v", err)
	}
	targetConfig.ConnConfig.Database = databaseName
	targetPool, err := pgxpool.NewWithConfig(ctx, targetConfig)
	if err != nil {
		t.Fatalf("连接未迁移测试数据库失败：%v", err)
	}
	_, applyErr := Apply(ctx, targetPool, uniqueIntegrationManifest())
	targetPool.Close()
	if applyErr == nil || !strings.Contains(applyErr.Error(), "migration") {
		t.Fatalf("未迁移数据库必须被明确拒绝：%v", applyErr)
	}
}

// uniqueIntegrationManifest 为共享测试数据库生成不会与其他测试冲突的完整 fixture。
func uniqueIntegrationManifest() Manifest {
	suffix := uuid.NewString()
	manifest := validTestManifest()
	manifest.OIDCIssuer = "https://idp.example.test/realms/" + suffix
	manifest.Tenant.ID = uuid.New()
	manifest.Tenant.Name = "OIDC bootstrap 测试租户 " + suffix
	manifest.Domain.ID = uuid.New()
	manifest.Domain.Name = "bootstrap-" + suffix + ".test"
	manifest.Mailbox.PrincipalID = uuid.New()
	manifest.Mailbox.MailboxID = uuid.New()
	manifest.Mailbox.Subject = "mailbox-" + suffix
	manifest.Administrator.PrincipalID = uuid.New()
	manifest.Administrator.Subject = "administrator-" + suffix
	manifest.Suspended.PrincipalID = uuid.New()
	manifest.Suspended.MailboxID = uuid.New()
	manifest.Suspended.Subject = "suspended-" + suffix
	manifest.MFAInsufficient.PrincipalID = uuid.New()
	manifest.MFAInsufficient.Subject = "mfa-insufficient-" + suffix
	manifest.UnmappedSubject = "unmapped-" + suffix
	return manifest
}

// assertFixtureCounts 核对 manifest 管理的各类控制面资源数量。
func assertFixtureCounts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	manifest Manifest,
	wantTenants int,
	wantDomains int,
	wantMailboxes int,
	wantPrincipals int,
	wantPermissions int,
) {
	t.Helper()
	var tenants int
	var domains int
	var mailboxes int
	var principals int
	var permissions int
	if err := pool.QueryRow(ctx, `
        SELECT
            (SELECT count(*) FROM tenants WHERE id = $1),
            (SELECT count(*) FROM domains WHERE id = $2),
            (SELECT count(*) FROM mailboxes WHERE id = ANY($3::uuid[])),
            (SELECT count(*) FROM identity_principals WHERE id = ANY($4::uuid[])),
            (SELECT count(*) FROM principal_permissions WHERE principal_id = ANY($4::uuid[]))
    `,
		manifest.Tenant.ID,
		manifest.Domain.ID,
		mailboxIDs(manifest),
		principalIDs(manifest),
	).Scan(&tenants, &domains, &mailboxes, &principals, &permissions); err != nil {
		t.Fatalf("统计测试身份 fixture 失败：%v", err)
	}
	got := []int{tenants, domains, mailboxes, principals, permissions}
	want := []int{wantTenants, wantDomains, wantMailboxes, wantPrincipals, wantPermissions}
	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("测试身份 fixture 数量错误：got=%v want=%v", got, want)
		}
	}
}

// assertUnmappedSubjectAbsent 确认未映射 Keycloak 身份没有本地授权主体。
func assertUnmappedSubjectAbsent(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	manifest Manifest,
) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
        SELECT count(*) FROM identity_principals
        WHERE oidc_issuer = $1 AND oidc_subject = $2
    `, manifest.OIDCIssuer, manifest.UnmappedSubject).Scan(&count); err != nil {
		t.Fatalf("读取未映射测试主体失败：%v", err)
	}
	if count != 0 {
		t.Fatal("unmapped 身份不得写入本地主体表")
	}
}

// insertSessionAndAudit 创建回收动作必须显式清理的测试会话和关联审计。
func insertSessionAndAudit(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	manifest Manifest,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessionDigest := sha256.Sum256([]byte("session-" + manifest.Mailbox.Subject))
	csrfDigest := sha256.Sum256([]byte("csrf-" + manifest.Mailbox.Subject))
	sourceDigest := sha256.Sum256([]byte("source-" + manifest.Mailbox.Subject))
	if _, err := pool.Exec(ctx, `
        INSERT INTO user_sessions (
            id, session_digest, principal_id, oidc_issuer, oidc_subject,
            csrf_digest, created_at, last_seen_at, idle_expires_at, absolute_expires_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8, $9)
    `,
		uuid.New(),
		sessionDigest[:],
		manifest.Mailbox.PrincipalID,
		manifest.OIDCIssuer,
		manifest.Mailbox.Subject,
		csrfDigest[:],
		now,
		now.Add(30*time.Minute),
		now.Add(8*time.Hour),
	); err != nil {
		t.Fatalf("创建待回收测试会话失败：%v", err)
	}
	if _, err := pool.Exec(ctx, `
        INSERT INTO authentication_audit_events (
            id, principal_id, action, result, source_digest, request_id, occurred_at
        ) VALUES ($1, $2, 'login', 'succeeded', $3, $4, $5)
    `,
		uuid.New(),
		manifest.Mailbox.PrincipalID,
		sourceDigest[:],
		"bootstrap-test-"+uuid.NewString(),
		now,
	); err != nil {
		t.Fatalf("创建待回收测试审计失败：%v", err)
	}
}

// forceDeleteIntegrationFixture 在断言失败后按测试 UUID 兜底清理共享数据库。
func forceDeleteIntegrationFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	manifest Manifest,
) {
	t.Helper()
	principals := principalIDs(manifest)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"DELETE FROM authentication_audit_events WHERE principal_id = ANY($1::uuid[])", []any{principals}},
		{"DELETE FROM user_sessions WHERE principal_id = ANY($1::uuid[])", []any{principals}},
		{"DELETE FROM principal_permissions WHERE principal_id = ANY($1::uuid[])", []any{principals}},
		{"DELETE FROM identity_principals WHERE id = ANY($1::uuid[])", []any{principals}},
		{"DELETE FROM mailboxes WHERE id = ANY($1::uuid[])", []any{mailboxIDs(manifest)}},
		{"DELETE FROM domains WHERE tenant_id = $1", []any{manifest.Tenant.ID}},
		{"DELETE FROM tenants WHERE id = $1", []any{manifest.Tenant.ID}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("兜底清理测试身份 fixture 失败：%v", err)
		}
	}
}
