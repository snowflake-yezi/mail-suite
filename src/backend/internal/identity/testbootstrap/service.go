package testbootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

const advisoryLockKey int64 = 0x4d53544944424f4f

var (
	// ErrDrift 表示数据库已有资源与 manifest 的稳定字段不一致。
	ErrDrift = errors.New("测试身份 fixture 存在数据漂移")
	// ErrUnsafeRemoval 表示 fixture 租户已承载 manifest 之外的数据，禁止自动回收。
	ErrUnsafeRemoval = errors.New("测试身份 fixture 包含不受 manifest 管理的数据")
)

// Result 描述初始化或回收是否实际修改了数据库，不暴露 fixture 标识或身份信息。
type Result struct {
	// Changed 表示本次事务创建或删除了至少一行数据。
	Changed bool
}

// provisionFixture 保存活动测试邮箱对应的确定性异步开通事实。
type provisionFixture struct {
	// OperationID 是活动测试邮箱唯一且可重复计算的开通任务标识。
	OperationID uuid.UUID
	// OutboxID 是该任务唯一且可重复计算的首发事件标识。
	OutboxID uuid.UUID
	// IdempotencyKey 防止重复 bootstrap 生成第二条业务任务。
	IdempotencyKey string
	// RequestHash 同时作为 operation 的不可变配置摘要。
	RequestHash [sha256.Size]byte
	// Payload 是不含地址和凭据的稳定 outbox 资源引用。
	Payload []byte
}

// Apply 在一个串行事务内创建或核验测试身份 fixture，发现漂移时不覆盖任何已有数据。
func Apply(ctx context.Context, pool *pgxpool.Pool, manifest Manifest) (Result, error) {
	if pool == nil {
		return Result{}, errors.New("测试身份初始化数据库连接不能为空")
	}
	if err := manifest.Validate(); err != nil {
		return Result{}, err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, databaseError()
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = prepareTransaction(ctx, tx); err != nil {
		return Result{}, err
	}

	changed, err := insertFixture(ctx, tx, manifest)
	if err != nil {
		return Result{}, err
	}
	if err = verifyFixture(ctx, tx, manifest); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, databaseError()
	}
	return Result{Changed: changed}, nil
}

// Remove 显式回收 manifest 精确拥有的测试资源，额外租户数据或字段漂移会阻止删除。
func Remove(ctx context.Context, pool *pgxpool.Pool, manifest Manifest) (Result, error) {
	if pool == nil {
		return Result{}, errors.New("测试身份回收数据库连接不能为空")
	}
	if err := manifest.Validate(); err != nil {
		return Result{}, err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, databaseError()
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = prepareTransaction(ctx, tx); err != nil {
		return Result{}, err
	}

	footprint, err := fixtureFootprint(ctx, tx, manifest)
	if err != nil {
		return Result{}, err
	}
	if footprint == 0 {
		if err = tx.Commit(ctx); err != nil {
			return Result{}, databaseError()
		}
		return Result{}, nil
	}
	if err = verifyFixture(ctx, tx, manifest); err != nil {
		return Result{}, err
	}
	if err = ensureRemovalScope(ctx, tx, manifest); err != nil {
		return Result{}, err
	}
	if err = deleteFixture(ctx, tx, manifest); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, databaseError()
	}
	return Result{Changed: true}, nil
}

// prepareTransaction 获取固定事务锁并确认认证 schema 已完成第二个 migration。
func prepareTransaction(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockKey); err != nil {
		return databaseError()
	}
	var schemaReady bool
	if err := tx.QueryRow(ctx, `
        SELECT to_regclass('public.goose_db_version') IS NOT NULL
           AND to_regclass('public.tenants') IS NOT NULL
           AND to_regclass('public.domains') IS NOT NULL
           AND to_regclass('public.mailboxes') IS NOT NULL
           AND to_regclass('public.identity_principals') IS NOT NULL
           AND to_regclass('public.principal_permissions') IS NOT NULL
           AND to_regclass('public.user_sessions') IS NOT NULL
           AND to_regclass('public.authentication_audit_events') IS NOT NULL
    `).Scan(&schemaReady); err != nil {
		return databaseError()
	}
	if !schemaReady {
		return errors.New("控制面数据库 schema 尚未完成身份 migration")
	}
	var migrationApplied bool
	if err := tx.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM goose_db_version WHERE version_id = 3 AND is_applied
        )
    `).Scan(&migrationApplied); err != nil {
		return databaseError()
	}
	if !migrationApplied {
		return errors.New("控制面数据库 schema 尚未完成 mailbox operation migration")
	}
	return nil
}

// insertFixture 只插入缺失行并累计变化，冲突行留给后续精确核验判定漂移。
func insertFixture(ctx context.Context, tx pgx.Tx, manifest Manifest) (bool, error) {
	provision, err := newProvisionFixture(manifest)
	if err != nil {
		return false, databaseError()
	}
	changed := false
	statements := []struct {
		query string
		args  []any
	}{
		{
			`INSERT INTO tenants (id, name, status) VALUES ($1, $2, 'active') ON CONFLICT DO NOTHING`,
			[]any{manifest.Tenant.ID, manifest.Tenant.Name},
		},
		{
			`INSERT INTO domains (id, tenant_id, name, status) VALUES ($1, $2, $3, 'active') ON CONFLICT DO NOTHING`,
			[]any{manifest.Domain.ID, manifest.Tenant.ID, manifest.Domain.Name},
		},
		{
			`INSERT INTO mailboxes (
                id, tenant_id, domain_id, local_part, display_name,
                desired_status, observed_status, revision
             ) VALUES ($1, $2, $3, $4, $5, 'active', 'unknown', 1)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.Mailbox.MailboxID,
				manifest.Tenant.ID,
				manifest.Domain.ID,
				manifest.Mailbox.LocalPart,
				manifest.Mailbox.DisplayName,
			},
		},
		{
			`INSERT INTO mailboxes (
                id, tenant_id, domain_id, local_part, display_name,
                desired_status, observed_status, revision
             ) VALUES ($1, $2, $3, $4, $5, 'active', 'unknown', 1)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.Suspended.MailboxID,
				manifest.Tenant.ID,
				manifest.Domain.ID,
				manifest.Suspended.LocalPart,
				manifest.Suspended.DisplayName,
			},
		},
		{
			`INSERT INTO operations (
                id, tenant_id, kind, resource_type, resource_id, status,
                idempotency_key, request_hash, desired_revision, configuration_hash
             ) VALUES ($1, $2, 'mailbox.provision', 'mailbox', $3, 'pending', $4, $5, 1, $5)
             ON CONFLICT DO NOTHING`,
			[]any{
				provision.OperationID,
				manifest.Tenant.ID,
				manifest.Mailbox.MailboxID,
				provision.IdempotencyKey,
				provision.RequestHash[:],
			},
		},
		{
			`INSERT INTO outbox_events (
                id, tenant_id, operation_id, event_type, payload, status, attempts
             ) VALUES ($1, $2, $3, 'mailbox.provision.requested', $4, 'pending', 0)
             ON CONFLICT DO NOTHING`,
			[]any{
				provision.OutboxID,
				manifest.Tenant.ID,
				provision.OperationID,
				provision.Payload,
			},
		},
		{
			`INSERT INTO identity_principals (
                id, tenant_id, oidc_issuer, oidc_subject, account_type,
                mailbox_id, status, display_name
             ) VALUES ($1, $2, $3, $4, 'mailbox', $5, 'active', $6)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.Mailbox.PrincipalID,
				manifest.Tenant.ID,
				manifest.OIDCIssuer,
				manifest.Mailbox.Subject,
				manifest.Mailbox.MailboxID,
				manifest.Mailbox.DisplayName,
			},
		},
		{
			`INSERT INTO identity_principals (
                id, tenant_id, oidc_issuer, oidc_subject, account_type,
                mailbox_id, status, display_name
             ) VALUES ($1, $2, $3, $4, 'administrator', NULL, 'active', $5)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.Administrator.PrincipalID,
				manifest.Tenant.ID,
				manifest.OIDCIssuer,
				manifest.Administrator.Subject,
				manifest.Administrator.DisplayName,
			},
		},
		{
			`INSERT INTO identity_principals (
                id, tenant_id, oidc_issuer, oidc_subject, account_type,
                mailbox_id, status, display_name
             ) VALUES ($1, $2, $3, $4, 'mailbox', $5, 'suspended', $6)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.Suspended.PrincipalID,
				manifest.Tenant.ID,
				manifest.OIDCIssuer,
				manifest.Suspended.Subject,
				manifest.Suspended.MailboxID,
				manifest.Suspended.DisplayName,
			},
		},
		{
			`INSERT INTO identity_principals (
                id, tenant_id, oidc_issuer, oidc_subject, account_type,
                mailbox_id, status, display_name
             ) VALUES ($1, $2, $3, $4, 'administrator', NULL, 'active', $5)
             ON CONFLICT DO NOTHING`,
			[]any{
				manifest.MFAInsufficient.PrincipalID,
				manifest.Tenant.ID,
				manifest.OIDCIssuer,
				manifest.MFAInsufficient.Subject,
				manifest.MFAInsufficient.DisplayName,
			},
		},
		{
			`INSERT INTO principal_permissions (principal_id, permission_name)
             VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			[]any{manifest.Administrator.PrincipalID, identity.PermissionPortalAdminAccess},
		},
		{
			`INSERT INTO principal_permissions (principal_id, permission_name)
             VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			[]any{manifest.MFAInsufficient.PrincipalID, identity.PermissionPortalAdminAccess},
		},
	}
	for _, statement := range statements {
		commandTag, err := tx.Exec(ctx, statement.query, statement.args...)
		if err != nil {
			return false, databaseError()
		}
		changed = changed || commandTag.RowsAffected() > 0
	}
	return changed, nil
}

// verifyFixture 对所有稳定字段和权限集合做精确核验，不覆盖不一致的已有数据。
func verifyFixture(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	if err := verifyTenant(ctx, tx, manifest); err != nil {
		return err
	}
	if err := verifyDomain(ctx, tx, manifest); err != nil {
		return err
	}
	if err := verifyMailbox(ctx, tx, manifest, manifest.Mailbox); err != nil {
		return err
	}
	if err := verifyMailbox(ctx, tx, manifest, manifest.Suspended); err != nil {
		return err
	}
	if err := verifyProvisionFixture(ctx, tx, manifest); err != nil {
		return err
	}
	if err := verifyPrincipal(
		ctx,
		tx,
		manifest,
		manifest.Mailbox.PrincipalID,
		manifest.Mailbox.Subject,
		"mailbox",
		manifest.Mailbox.MailboxID.String(),
		"active",
		manifest.Mailbox.DisplayName,
	); err != nil {
		return err
	}
	if err := verifyPrincipal(
		ctx,
		tx,
		manifest,
		manifest.Administrator.PrincipalID,
		manifest.Administrator.Subject,
		"administrator",
		"",
		"active",
		manifest.Administrator.DisplayName,
	); err != nil {
		return err
	}
	if err := verifyPrincipal(
		ctx,
		tx,
		manifest,
		manifest.Suspended.PrincipalID,
		manifest.Suspended.Subject,
		"mailbox",
		manifest.Suspended.MailboxID.String(),
		"suspended",
		manifest.Suspended.DisplayName,
	); err != nil {
		return err
	}
	if err := verifyPrincipal(
		ctx,
		tx,
		manifest,
		manifest.MFAInsufficient.PrincipalID,
		manifest.MFAInsufficient.Subject,
		"administrator",
		"",
		"active",
		manifest.MFAInsufficient.DisplayName,
	); err != nil {
		return err
	}

	permissionExpectations := []struct {
		principalID uuid.UUID
		permissions []string
	}{
		{manifest.Mailbox.PrincipalID, nil},
		{manifest.Administrator.PrincipalID, []string{identity.PermissionPortalAdminAccess}},
		{manifest.Suspended.PrincipalID, nil},
		{manifest.MFAInsufficient.PrincipalID, []string{identity.PermissionPortalAdminAccess}},
	}
	for _, expectation := range permissionExpectations {
		permissions, err := readPermissions(ctx, tx, expectation.principalID)
		if err != nil {
			return err
		}
		if !slices.Equal(permissions, expectation.permissions) {
			return driftError("主体权限")
		}
	}

	var unmappedExists bool
	if err := tx.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM identity_principals
            WHERE oidc_issuer = $1 AND oidc_subject = $2
        )
    `, manifest.OIDCIssuer, manifest.UnmappedSubject).Scan(&unmappedExists); err != nil {
		return databaseError()
	}
	if unmappedExists {
		return driftError("未映射主体")
	}
	return nil
}

// verifyTenant 核验 fixture 租户的稳定字段。
func verifyTenant(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	var name string
	var status string
	if err := tx.QueryRow(
		ctx,
		"SELECT name, status FROM tenants WHERE id = $1",
		manifest.Tenant.ID,
	).Scan(&name, &status); err != nil {
		return rowVerificationError(err, "租户")
	}
	if name != manifest.Tenant.Name || status != "active" {
		return driftError("租户")
	}
	return nil
}

// verifyDomain 核验 fixture 域名的租户归属、名称和状态。
func verifyDomain(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	var tenantID uuid.UUID
	var name string
	var status string
	if err := tx.QueryRow(
		ctx,
		"SELECT tenant_id, name, status FROM domains WHERE id = $1",
		manifest.Domain.ID,
	).Scan(&tenantID, &name, &status); err != nil {
		return rowVerificationError(err, "域名")
	}
	if tenantID != manifest.Tenant.ID || name != manifest.Domain.Name || status != "active" {
		return driftError("域名")
	}
	return nil
}

// verifyMailbox 核验邮箱的控制面 owner 字段，允许 observed_status 由 mail-core 合法推进。
func verifyMailbox(
	ctx context.Context,
	tx pgx.Tx,
	manifest Manifest,
	mailbox MailboxPrincipalManifest,
) error {
	var tenantID uuid.UUID
	var domainID uuid.UUID
	var localPart string
	var displayName string
	var desiredStatus string
	var revision int64
	if err := tx.QueryRow(ctx, `
        SELECT tenant_id, domain_id, local_part, COALESCE(display_name, ''), desired_status, revision
        FROM mailboxes WHERE id = $1
    `, mailbox.MailboxID).Scan(
		&tenantID,
		&domainID,
		&localPart,
		&displayName,
		&desiredStatus,
		&revision,
	); err != nil {
		return rowVerificationError(err, "邮箱")
	}
	if tenantID != manifest.Tenant.ID || domainID != manifest.Domain.ID ||
		localPart != mailbox.LocalPart || displayName != mailbox.DisplayName ||
		desiredStatus != "active" || revision != 1 {
		return driftError("邮箱")
	}
	return nil
}

// verifyProvisionFixture 核验活动邮箱的 operation/outbox 所有权字段并允许运行状态合法推进。
func verifyProvisionFixture(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	provision, err := newProvisionFixture(manifest)
	if err != nil {
		return databaseError()
	}
	var operationMatches bool
	if err = tx.QueryRow(ctx, `
        SELECT tenant_id = $2
           AND kind = 'mailbox.provision'
           AND resource_type = 'mailbox'
           AND resource_id = $3
           AND idempotency_key = $4
           AND request_hash = $5
           AND desired_revision = 1
           AND configuration_hash = $5
           AND status IN ('pending', 'running', 'retry_wait', 'unknown', 'succeeded', 'failed', 'dead', 'superseded')
        FROM operations
        WHERE id = $1
    `,
		provision.OperationID,
		manifest.Tenant.ID,
		manifest.Mailbox.MailboxID,
		provision.IdempotencyKey,
		provision.RequestHash[:],
	).Scan(&operationMatches); err != nil {
		return rowVerificationError(err, "邮箱开通 operation")
	}
	if !operationMatches {
		return driftError("邮箱开通 operation")
	}

	var outboxMatches bool
	if err = tx.QueryRow(ctx, `
        SELECT tenant_id = $2
           AND operation_id = $3
           AND event_type = 'mailbox.provision.requested'
           AND payload = $4::jsonb
           AND status IN ('pending', 'processing', 'delivered', 'dead')
        FROM outbox_events
        WHERE id = $1
    `,
		provision.OutboxID,
		manifest.Tenant.ID,
		provision.OperationID,
		string(provision.Payload),
	).Scan(&outboxMatches); err != nil {
		return rowVerificationError(err, "邮箱开通 outbox")
	}
	if !outboxMatches {
		return driftError("邮箱开通 outbox")
	}
	return nil
}

// verifyPrincipal 核验本地主体授权映射的全部稳定字段。
func verifyPrincipal(
	ctx context.Context,
	tx pgx.Tx,
	manifest Manifest,
	principalID uuid.UUID,
	subject string,
	accountType string,
	mailboxID string,
	status string,
	displayName string,
) error {
	var actualTenantID uuid.UUID
	var actualIssuer string
	var actualSubject string
	var actualAccountType string
	var actualMailboxID string
	var actualStatus string
	var actualDisplayName string
	if err := tx.QueryRow(ctx, `
        SELECT tenant_id, oidc_issuer, oidc_subject, account_type,
               COALESCE(mailbox_id::text, ''), status, display_name
        FROM identity_principals WHERE id = $1
    `, principalID).Scan(
		&actualTenantID,
		&actualIssuer,
		&actualSubject,
		&actualAccountType,
		&actualMailboxID,
		&actualStatus,
		&actualDisplayName,
	); err != nil {
		return rowVerificationError(err, "本地主体")
	}
	if actualTenantID != manifest.Tenant.ID || actualIssuer != manifest.OIDCIssuer ||
		actualSubject != subject || actualAccountType != accountType || actualMailboxID != mailboxID ||
		actualStatus != status || actualDisplayName != displayName {
		return driftError("本地主体")
	}
	return nil
}

// readPermissions 返回稳定排序后的主体权限集合。
func readPermissions(ctx context.Context, tx pgx.Tx, principalID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `
        SELECT permission_name FROM principal_permissions
        WHERE principal_id = $1 ORDER BY permission_name
    `, principalID)
	if err != nil {
		return nil, databaseError()
	}
	permissions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, databaseError()
	}
	return permissions, nil
}

// fixtureFootprint 判断任一稳定 ID、自然键或外部身份键是否仍有数据库占位。
func fixtureFootprint(ctx context.Context, tx pgx.Tx, manifest Manifest) (int, error) {
	provision, err := newProvisionFixture(manifest)
	if err != nil {
		return 0, databaseError()
	}
	var count int
	if err = tx.QueryRow(ctx, `
        SELECT
            (SELECT count(*) FROM tenants WHERE id = $1) +
            (SELECT count(*) FROM domains WHERE id = $2 OR name = $3) +
            (SELECT count(*) FROM mailboxes
             WHERE id = ANY($4::uuid[])
                OR (domain_id = $2 AND local_part = ANY($5::text[]))) +
            (SELECT count(*) FROM identity_principals
             WHERE id = ANY($6::uuid[])
                OR (oidc_issuer = $7 AND oidc_subject = ANY($8::text[]))) +
            (SELECT count(*) FROM principal_permissions WHERE principal_id = ANY($6::uuid[])) +
            (SELECT count(*) FROM operations
             WHERE id = $9 OR (tenant_id = $1 AND idempotency_key = $10)) +
            (SELECT count(*) FROM outbox_events
             WHERE id = $11 OR operation_id = $9)
    `,
		manifest.Tenant.ID,
		manifest.Domain.ID,
		manifest.Domain.Name,
		mailboxIDs(manifest),
		[]string{manifest.Mailbox.LocalPart, manifest.Suspended.LocalPart},
		principalIDs(manifest),
		manifest.OIDCIssuer,
		allSubjects(manifest),
		provision.OperationID,
		provision.IdempotencyKey,
		provision.OutboxID,
	).Scan(&count); err != nil {
		return 0, databaseError()
	}
	return count, nil
}

// ensureRemovalScope 拒绝删除已承载任何 manifest 外业务资源的测试租户。
func ensureRemovalScope(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	provision, err := newProvisionFixture(manifest)
	if err != nil {
		return databaseError()
	}
	var hasExtraData bool
	if err = tx.QueryRow(ctx, `
        SELECT
            EXISTS (SELECT 1 FROM domains WHERE tenant_id = $1 AND id <> $2)
            OR EXISTS (SELECT 1 FROM mailboxes WHERE tenant_id = $1 AND id <> ALL($3::uuid[]))
            OR EXISTS (SELECT 1 FROM identity_principals WHERE tenant_id = $1 AND id <> ALL($4::uuid[]))
            OR EXISTS (SELECT 1 FROM operations WHERE tenant_id = $1 AND id <> $5)
            OR EXISTS (SELECT 1 FROM outbox_events WHERE tenant_id = $1 AND id <> $6)
            OR EXISTS (SELECT 1 FROM operation_attempts WHERE tenant_id = $1 AND operation_id <> $5)
    `,
		manifest.Tenant.ID,
		manifest.Domain.ID,
		mailboxIDs(manifest),
		principalIDs(manifest),
		provision.OperationID,
		provision.OutboxID,
	).Scan(&hasExtraData); err != nil {
		return databaseError()
	}
	if hasExtraData {
		return ErrUnsafeRemoval
	}
	return nil
}

// deleteFixture 按外键逆序删除 fixture 拥有的数据，并核验稳定资源删除数量。
func deleteFixture(ctx context.Context, tx pgx.Tx, manifest Manifest) error {
	provision, err := newProvisionFixture(manifest)
	if err != nil {
		return databaseError()
	}
	principals := principalIDs(manifest)
	statements := []struct {
		query        string
		args         []any
		wantAffected int64
	}{
		{"DELETE FROM authentication_audit_events WHERE principal_id = ANY($1::uuid[])", []any{principals}, -1},
		{"DELETE FROM user_sessions WHERE principal_id = ANY($1::uuid[])", []any{principals}, -1},
		{"DELETE FROM principal_permissions WHERE principal_id = ANY($1::uuid[])", []any{principals}, 2},
		{"DELETE FROM identity_principals WHERE id = ANY($1::uuid[])", []any{principals}, 4},
		{"DELETE FROM outbox_events WHERE id = $1", []any{provision.OutboxID}, 1},
		{"DELETE FROM operation_attempts WHERE operation_id = $1", []any{provision.OperationID}, -1},
		{"DELETE FROM operations WHERE id = $1", []any{provision.OperationID}, 1},
		{"DELETE FROM mailboxes WHERE id = ANY($1::uuid[])", []any{mailboxIDs(manifest)}, 2},
		{"DELETE FROM domains WHERE id = $1", []any{manifest.Domain.ID}, 1},
		{"DELETE FROM tenants WHERE id = $1", []any{manifest.Tenant.ID}, 1},
	}
	for _, statement := range statements {
		commandTag, err := tx.Exec(ctx, statement.query, statement.args...)
		if err != nil {
			return databaseError()
		}
		if statement.wantAffected >= 0 && commandTag.RowsAffected() != statement.wantAffected {
			return driftError("回收行数")
		}
	}
	return nil
}

// principalIDs 返回 manifest 管理的全部本地主体标识。
func principalIDs(manifest Manifest) []uuid.UUID {
	return []uuid.UUID{
		manifest.Mailbox.PrincipalID,
		manifest.Administrator.PrincipalID,
		manifest.Suspended.PrincipalID,
		manifest.MFAInsufficient.PrincipalID,
	}
}

// mailboxIDs 返回 manifest 管理的两个测试邮箱标识。
func mailboxIDs(manifest Manifest) []uuid.UUID {
	return []uuid.UUID{manifest.Mailbox.MailboxID, manifest.Suspended.MailboxID}
}

// newProvisionFixture 从活动邮箱稳定身份生成 operation、outbox 和业务配置摘要。
func newProvisionFixture(manifest Manifest) (provisionFixture, error) {
	operationID := uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte("mail-suite:test-bootstrap:operation:"+manifest.Mailbox.MailboxID.String()),
	)
	outboxID := uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte("mail-suite:test-bootstrap:outbox:"+manifest.Mailbox.MailboxID.String()),
	)
	fingerprint, err := json.Marshal(struct {
		DomainID    string `json:"domain_id"`
		LocalPart   string `json:"local_part"`
		DisplayName string `json:"display_name"`
	}{
		DomainID:    manifest.Domain.ID.String(),
		LocalPart:   manifest.Mailbox.LocalPart,
		DisplayName: manifest.Mailbox.DisplayName,
	})
	if err != nil {
		return provisionFixture{}, err
	}
	payload, err := json.Marshal(struct {
		TenantID    uuid.UUID `json:"tenant_id"`
		MailboxID   uuid.UUID `json:"mailbox_id"`
		DomainID    uuid.UUID `json:"domain_id"`
		OperationID uuid.UUID `json:"operation_id"`
		Revision    int64     `json:"revision"`
	}{
		TenantID:    manifest.Tenant.ID,
		MailboxID:   manifest.Mailbox.MailboxID,
		DomainID:    manifest.Domain.ID,
		OperationID: operationID,
		Revision:    1,
	})
	if err != nil {
		return provisionFixture{}, err
	}
	return provisionFixture{
		OperationID:    operationID,
		OutboxID:       outboxID,
		IdempotencyKey: "test-bootstrap-mailbox:" + manifest.Mailbox.MailboxID.String(),
		RequestHash:    sha256.Sum256(fingerprint),
		Payload:        payload,
	}, nil
}

// allSubjects 返回五类测试身份的 OIDC subject，用于冲突足迹检查。
func allSubjects(manifest Manifest) []string {
	return []string{
		manifest.Mailbox.Subject,
		manifest.Administrator.Subject,
		manifest.Suspended.Subject,
		manifest.MFAInsufficient.Subject,
		manifest.UnmappedSubject,
	}
}

// rowVerificationError 把缺失行归类为漂移，并隐藏其他数据库实现错误。
func rowVerificationError(err error, resource string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return driftError(resource)
	}
	return databaseError()
}

// driftError 返回不包含 fixture 值的稳定漂移类别。
func driftError(resource string) error {
	return fmt.Errorf("%w：%s", ErrDrift, resource)
}

// databaseError 统一隐藏连接、SQL 和数据库值，避免错误输出泄露运行配置。
func databaseError() error {
	return errors.New("测试身份数据库操作失败")
}
