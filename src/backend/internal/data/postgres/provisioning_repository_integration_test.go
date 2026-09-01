package postgres

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/provisioning"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

func TestProvisioningRepositoryPersistsIdempotentTenantScopedIntent(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过邮箱开通真实 PostgreSQL 集成测试")
	}
	ctx := context.Background()

	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("应用测试 migration 失败：%v", err)
	}
	var secondUp bytes.Buffer
	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, &secondUp); err != nil {
		t.Fatalf("重复应用 migration 失败：%v", err)
	}
	if secondUp.String() != "数据库 schema 已是最新版本\n" {
		t.Fatalf("重复 migration 输出不稳定：%q", secondUp.String())
	}
	var migrationStatus bytes.Buffer
	if err := migrations.Run(ctx, databaseURL, migrations.CommandStatus, &migrationStatus); err != nil {
		t.Fatalf("读取 migration 状态失败：%v", err)
	}
	if migrationStatus.String() != "00001 applied\n00002 applied\n" {
		t.Fatalf("migration 状态输出不稳定：%q", migrationStatus.String())
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建测试连接池失败：%v", err)
	}
	t.Cleanup(pool.Close)

	tenantID := uuid.New()
	otherTenantID := uuid.New()
	domainID := uuid.New()
	inactiveDomainID := uuid.New()
	otherDomainID := uuid.New()
	insertProvisioningFixtures(t, ctx, pool, tenantID, otherTenantID, domainID, inactiveDomainID, otherDomainID)
	t.Cleanup(func() {
		deleteProvisioningFixtures(t, ctx, pool, tenantID, otherTenantID)
	})

	service := provisioning.NewService(NewProvisioningRepository(pool))
	command := provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "Alice",
		DisplayName:    " Alice ",
		IdempotencyKey: "request-alice-0001",
	}
	created, err := service.RequestMailbox(ctx, command)
	if err != nil {
		t.Fatalf("持久化邮箱开通意图失败：%v", err)
	}
	if created.Replayed || created.MailboxID == uuid.Nil || created.OperationID == uuid.Nil {
		t.Fatalf("首次创建结果无效：%+v", created)
	}
	assertProvisioningRowCounts(t, ctx, pool, tenantID, 1)

	replayed, err := service.RequestMailbox(ctx, command)
	if err != nil {
		t.Fatalf("幂等重放失败：%v", err)
	}
	if !replayed.Replayed || replayed.MailboxID != created.MailboxID || replayed.OperationID != created.OperationID {
		t.Fatalf("幂等重放未返回原标识：created=%+v replayed=%+v", created, replayed)
	}
	assertProvisioningRowCounts(t, ctx, pool, tenantID, 1)

	conflicting := command
	conflicting.LocalPart = "bob"
	_, err = service.RequestMailbox(ctx, conflicting)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeIdempotencyConflict) {
		t.Fatalf("幂等键复用应冲突，实际为 %v", err)
	}

	duplicate := command
	duplicate.IdempotencyKey = "request-alice-0002"
	_, err = service.RequestMailbox(ctx, duplicate)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeMailboxAlreadyExists) {
		t.Fatalf("重复邮箱应返回已存在，实际为 %v", err)
	}
	assertProvisioningRowCounts(t, ctx, pool, tenantID, 1)

	inactive := command
	inactive.DomainID = inactiveDomainID
	inactive.LocalPart = "inactive"
	inactive.IdempotencyKey = "request-inactive-01"
	_, err = service.RequestMailbox(ctx, inactive)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeDomainUnavailable) {
		t.Fatalf("停用域名应不可用，实际为 %v", err)
	}

	crossTenant := command
	crossTenant.DomainID = otherDomainID
	crossTenant.LocalPart = "cross"
	crossTenant.IdempotencyKey = "request-cross-0001"
	_, err = service.RequestMailbox(ctx, crossTenant)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeDomainUnavailable) {
		t.Fatalf("跨租户域名应统一为不可用，实际为 %v", err)
	}
	assertProvisioningRowCounts(t, ctx, pool, tenantID, 1)

	operation, err := service.GetOperation(ctx, tenantID, created.OperationID)
	if err != nil || operation.Status != "pending" || operation.ResourceID != created.MailboxID {
		t.Fatalf("租户内 operation 查询失败：operation=%+v err=%v", operation, err)
	}
	_, err = service.GetOperation(ctx, otherTenantID, created.OperationID)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeOperationNotFound) {
		t.Fatalf("跨租户 operation 查询应返回不存在，实际为 %v", err)
	}

	assertOutboxTenantConstraint(t, ctx, pool, created.OperationID, tenantID, otherTenantID)
	assertSchemaComments(t, ctx, pool)
}

// insertProvisioningFixtures 创建彼此隔离的租户与域名，不写入任何真实业务数据。
func insertProvisioningFixtures(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	otherTenantID uuid.UUID,
	domainID uuid.UUID,
	inactiveDomainID uuid.UUID,
	otherDomainID uuid.UUID,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
        INSERT INTO tenants (id, name, status)
        VALUES ($1, '集成测试租户一', 'active'), ($2, '集成测试租户二', 'active')
    `, tenantID, otherTenantID); err != nil {
		t.Fatalf("创建测试租户失败：%v", err)
	}
	if _, err := pool.Exec(ctx, `
        INSERT INTO domains (id, tenant_id, name, status)
        VALUES
            ($1, $2, $3, 'active'),
            ($4, $2, $5, 'suspended'),
            ($6, $7, $8, 'active')
    `,
		domainID,
		tenantID,
		"active-"+domainID.String()+".test",
		inactiveDomainID,
		"inactive-"+inactiveDomainID.String()+".test",
		otherDomainID,
		otherTenantID,
		"other-"+otherDomainID.String()+".test",
	); err != nil {
		t.Fatalf("创建测试域名失败：%v", err)
	}
}

// deleteProvisioningFixtures 按本测试生成的租户标识逆序清理持久化数据。
func deleteProvisioningFixtures(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	otherTenantID uuid.UUID,
) {
	t.Helper()
	for _, statement := range []string{
		"DELETE FROM outbox_events WHERE tenant_id IN ($1, $2)",
		"DELETE FROM operations WHERE tenant_id IN ($1, $2)",
		"DELETE FROM mailboxes WHERE tenant_id IN ($1, $2)",
		"DELETE FROM domains WHERE tenant_id IN ($1, $2)",
		"DELETE FROM tenants WHERE id IN ($1, $2)",
	} {
		if _, err := pool.Exec(ctx, statement, tenantID, otherTenantID); err != nil {
			t.Errorf("清理邮箱开通测试数据失败：%v", err)
		}
	}
}

// assertProvisioningRowCounts 核对失败事务没有留下孤立 operation 或 outbox。
func assertProvisioningRowCounts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	want int,
) {
	t.Helper()
	var mailboxCount int
	var operationCount int
	var outboxCount int
	if err := pool.QueryRow(ctx, `
        SELECT
            (SELECT count(*) FROM mailboxes WHERE tenant_id = $1),
            (SELECT count(*) FROM operations WHERE tenant_id = $1),
            (SELECT count(*) FROM outbox_events WHERE tenant_id = $1)
    `, tenantID).Scan(&mailboxCount, &operationCount, &outboxCount); err != nil {
		t.Fatalf("统计邮箱开通事务行数失败：%v", err)
	}
	counts := map[string]int{
		"mailboxes":     mailboxCount,
		"operations":    operationCount,
		"outbox_events": outboxCount,
	}
	for table, count := range counts {
		if count != want {
			t.Fatalf("%s 行数不正确：got=%d want=%d", table, count, want)
		}
	}
}

// assertOutboxTenantConstraint 验证数据库拒绝把 operation 的 outbox 改到其他租户。
func assertOutboxTenantConstraint(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	operationID uuid.UUID,
	tenantID uuid.UUID,
	otherTenantID uuid.UUID,
) {
	t.Helper()
	if _, err := pool.Exec(
		ctx,
		"UPDATE outbox_events SET tenant_id = $1 WHERE operation_id = $2",
		otherTenantID,
		operationID,
	); err == nil {
		t.Fatal("数据库必须拒绝 outbox 与 operation 租户不一致")
	}
	var persistedTenantID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		"SELECT tenant_id FROM outbox_events WHERE operation_id = $1",
		operationID,
	).Scan(&persistedTenantID); err != nil {
		t.Fatalf("读取 outbox 租户失败：%v", err)
	}
	if persistedTenantID != tenantID {
		t.Fatalf("失败更新后 outbox 租户发生变化：got=%s want=%s", persistedTenantID, tenantID)
	}
}

// assertSchemaComments 确认全部业务表、字段和 Goose 元数据都有中文语义注释。
func assertSchemaComments(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tables := []string{"goose_db_version", "tenants", "domains", "mailboxes", "operations", "outbox_events"}
	var tableCount int
	var missingTableComments int
	if err := pool.QueryRow(ctx, `
        SELECT count(*), count(*) FILTER (
            WHERE obj_description(
                to_regclass(format('%I.%I', tables.table_schema, tables.table_name)),
                'pg_class'
            ) IS NULL
        )
        FROM information_schema.tables AS tables
        WHERE tables.table_schema = current_schema()
          AND tables.table_name = ANY($1::text[])
    `, tables).Scan(&tableCount, &missingTableComments); err != nil {
		t.Fatalf("检查表注释失败：%v", err)
	}
	if tableCount != len(tables) {
		t.Fatalf("schema 表数量不完整：got=%d want=%d", tableCount, len(tables))
	}
	if missingTableComments != 0 {
		t.Fatalf("存在 %d 个缺失注释的表", missingTableComments)
	}

	var missingColumnComments int
	if err := pool.QueryRow(ctx, `
        SELECT count(*)
        FROM information_schema.columns AS columns
        WHERE columns.table_schema = current_schema()
          AND columns.table_name = ANY($1::text[])
          AND col_description(
              to_regclass(format('%I.%I', columns.table_schema, columns.table_name)),
              columns.ordinal_position
          ) IS NULL
    `, tables).Scan(&missingColumnComments); err != nil {
		t.Fatalf("检查字段注释失败：%v", err)
	}
	if missingColumnComments != 0 {
		t.Fatalf("存在 %d 个缺失注释的字段", missingColumnComments)
	}
}
