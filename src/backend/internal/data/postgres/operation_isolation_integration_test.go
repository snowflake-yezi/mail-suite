package postgres

import (
	"context"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	operationdomain "github.com/snowflake-yezi/mail-suite/src/backend/internal/operation"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/provisioning"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

// isolatedOperationDatabaseURL 为全局任务扫描测试创建独立 schema，并在连接池关闭后清理。
func isolatedOperationDatabaseURL(t *testing.T, databaseURL string) string {
	t.Helper()
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建隔离 schema 管理连接池失败：%v", err)
	}
	t.Cleanup(adminPool.Close)
	schemaName := "mail_suite_worker_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	if _, err = adminPool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema 失败：%v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, cleanupErr := adminPool.Exec(cleanupContext, "DROP SCHEMA "+quotedSchema+" CASCADE"); cleanupErr != nil {
			t.Errorf("清理隔离 schema 失败：%v", cleanupErr)
		}
	})
	parsedURL, err := url.Parse(databaseURL)
	if err != nil || parsedURL.Scheme == "" {
		t.Fatalf("测试数据库必须使用 PostgreSQL URL：%v", err)
	}
	query := parsedURL.Query()
	query.Set("search_path", schemaName)
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String()
}

// TestOperationClaimsDoNotConsumeAnotherTestSchema 验证其他测试已有可领取任务时，空队列仍保持为空。
func TestOperationClaimsDoNotConsumeAnotherTestSchema(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过任务扫描隔离回归")
	}
	ctx := context.Background()
	// 两个 schema 模拟并行测试包，使用相同数据库但各自拥有任务队列。
	pools := make([]*pgxpool.Pool, 2)
	for index := range pools {
		isolatedURL := isolatedOperationDatabaseURL(t, databaseURL)
		if err := migrations.Run(ctx, isolatedURL, migrations.CommandUp, io.Discard); err != nil {
			t.Fatalf("应用隔离 schema migration 失败：%v", err)
		}
		pool, err := pgxpool.New(ctx, isolatedURL)
		if err != nil {
			t.Fatalf("创建隔离测试连接池失败：%v", err)
		}
		t.Cleanup(pool.Close)
		pools[index] = pool
	}
	tenantID, domainID := uuid.New(), uuid.New()
	insertProvisioningFixtures(t, ctx, pools[0], tenantID, uuid.New(), domainID, uuid.New(), uuid.New())
	ownerRepository := NewProvisioningRepository(pools[0])
	created, err := provisioning.NewService(ownerRepository).RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID: tenantID, DomainID: domainID, LocalPart: "schema-isolation",
		DisplayName: "Schema Isolation", IdempotencyKey: "schema-isolation-request-0001",
	})
	if err != nil {
		t.Fatalf("创建干扰任务失败：%v", err)
	}
	request := operationdomain.ClaimRequest{OwnerID: uuid.New(), LeaseDuration: time.Minute, MaxAttempts: 3}
	if claim, found, claimErr := NewProvisioningRepository(pools[1]).ClaimMailboxOperation(ctx, request); claimErr != nil || found {
		t.Fatalf("空 schema 不得领取其他测试的任务：operation=%s found=%t err=%v", claim.OperationID, found, claimErr)
	}
	claim, found, err := ownerRepository.ClaimMailboxOperation(ctx, request)
	if err != nil || !found || claim.OperationID != created.OperationID || claim.AttemptNumber != 1 {
		t.Fatalf("原 schema 的任务必须保持可首次领取：claim=%+v found=%t err=%v", claim, found, err)
	}
}
