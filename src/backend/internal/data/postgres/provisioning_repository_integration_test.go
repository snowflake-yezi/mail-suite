package postgres

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
	operationdomain "github.com/snowflake-yezi/mail-suite/src/backend/internal/operation"
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
	if migrationStatus.String() != "00001 applied\n00002 applied\n00003 applied\n" {
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

func TestOperationRepositoryFencesConcurrentClaimsAndRecoversExpiredLease(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过 operation worker 真实 PostgreSQL 集成测试")
	}
	// 领取会扫描全部租户，必须隔离其他测试包创建的待开通任务。
	databaseURL = isolatedOperationDatabaseURL(t, databaseURL)
	ctx := context.Background()
	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("应用 operation worker migration 失败：%v", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建 operation worker 测试连接池失败：%v", err)
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

	repository := NewProvisioningRepository(pool)
	service := provisioning.NewService(repository)
	firstCreated, err := service.RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "worker-first",
		DisplayName:    "Worker First",
		IdempotencyKey: "worker-first-request-0001",
	})
	if err != nil {
		t.Fatalf("创建并发领取测试 operation 失败：%v", err)
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE operations SET status = 'succeeded' WHERE id = $1",
		firstCreated.OperationID,
	); err == nil {
		t.Fatal("数据库必须拒绝没有 inspect 证据的 succeeded operation")
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE mailboxes SET observed_status = 'active' WHERE id = $1",
		firstCreated.MailboxID,
	); err == nil {
		t.Fatal("数据库必须拒绝没有 revision/hash 证据的 active mailbox")
	}

	type claimResult struct {
		claimed operationdomain.ClaimedMailboxOperation
		found   bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	owners := []uuid.UUID{uuid.New(), uuid.New()}
	var claimers sync.WaitGroup
	for _, ownerID := range owners {
		claimers.Add(1)
		go func(ownerID uuid.UUID) {
			defer claimers.Done()
			<-start
			claimed, found, claimErr := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
				OwnerID:       ownerID,
				LeaseDuration: time.Second,
				MaxAttempts:   3,
			})
			results <- claimResult{claimed: claimed, found: found, err: claimErr}
		}(ownerID)
	}
	close(start)
	claimers.Wait()
	close(results)

	var firstClaim operationdomain.ClaimedMailboxOperation
	foundCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("并发领取返回错误：%v", result.err)
		}
		if result.found {
			firstClaim = result.claimed
			foundCount++
		}
	}
	if foundCount != 1 || firstClaim.OperationID != firstCreated.OperationID ||
		firstClaim.MailboxID != firstCreated.MailboxID || firstClaim.AttemptNumber != 1 ||
		firstClaim.LeaseEpoch != 1 || firstClaim.PriorStatus != operationdomain.StatusPending ||
		len(firstClaim.ConfigurationHash) != 32 {
		t.Fatalf("并发领取必须只产生一个有效 attempt：count=%d claim=%+v", foundCount, firstClaim)
	}

	assertClaimPersistence(t, ctx, pool, firstClaim.OperationID, "running", 1, 1, "delivered", 1)
	staleResolution := operationdomain.Resolution{
		OperationID:      firstClaim.OperationID,
		AttemptNumber:    firstClaim.AttemptNumber,
		LeaseOwnerID:     firstClaim.LeaseOwnerID,
		LeaseEpoch:       firstClaim.LeaseEpoch + 1,
		DesiredRevision:  firstClaim.DesiredRevision,
		Status:           operationdomain.StatusUnknown,
		NextAttemptDelay: time.Millisecond,
		ErrorCode:        mailcore.ErrorCodeUnknown,
	}
	if err = repository.ResolveMailboxOperation(ctx, staleResolution); !errors.Is(err, operationdomain.ErrLeaseLost) {
		t.Fatalf("错误 epoch 回执必须被 fencing：%v", err)
	}
	assertClaimPersistence(t, ctx, pool, firstClaim.OperationID, "running", 1, 1, "delivered", 1)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	successResolution := operationdomain.Resolution{
		OperationID:     firstClaim.OperationID,
		AttemptNumber:   firstClaim.AttemptNumber,
		LeaseOwnerID:    firstClaim.LeaseOwnerID,
		LeaseEpoch:      firstClaim.LeaseEpoch,
		DesiredRevision: firstClaim.DesiredRevision,
		Status:          operationdomain.StatusSucceeded,
		Observation: &mailcore.ObservedMailbox{
			MailboxID:         firstClaim.MailboxID,
			Status:            mailcore.MailboxStatusActive,
			Revision:          firstClaim.DesiredRevision,
			ConfigurationHash: firstClaim.ConfigurationHash,
			InspectedAt:       observedAt,
		},
	}
	if _, err = pool.Exec(ctx, "UPDATE mailboxes SET revision = 2 WHERE id = $1", firstClaim.MailboxID); err != nil {
		t.Fatalf("准备 mailbox revision CAS 测试失败：%v", err)
	}
	if err = repository.ResolveMailboxOperation(ctx, successResolution); !errors.Is(err, operationdomain.ErrLeaseLost) {
		t.Fatalf("邮箱期望 revision 已变化时成功回执必须被 fencing：%v", err)
	}
	assertClaimPersistence(t, ctx, pool, firstClaim.OperationID, "running", 1, 1, "delivered", 1)
	if _, err = pool.Exec(ctx, "UPDATE mailboxes SET revision = 1 WHERE id = $1", firstClaim.MailboxID); err != nil {
		t.Fatalf("恢复 mailbox revision 测试状态失败：%v", err)
	}
	if err = repository.ResolveMailboxOperation(ctx, successResolution); err != nil {
		t.Fatalf("提交匹配实际观测的成功回执失败：%v", err)
	}
	assertSuccessfulOperation(t, ctx, pool, firstClaim, observedAt)

	secondCreated, err := service.RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "worker-takeover",
		DisplayName:    "Worker Takeover",
		IdempotencyKey: "worker-takeover-request-0001",
	})
	if err != nil {
		t.Fatalf("创建租约接管测试 operation 失败：%v", err)
	}
	oldClaim, found, err := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID:       uuid.New(),
		LeaseDuration: 5 * time.Millisecond,
		MaxAttempts:   3,
	})
	if err != nil || !found || oldClaim.OperationID != secondCreated.OperationID {
		t.Fatalf("领取短租约 operation 失败：claim=%+v found=%t err=%v", oldClaim, found, err)
	}
	time.Sleep(25 * time.Millisecond)
	newClaim, found, err := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID:       uuid.New(),
		LeaseDuration: time.Second,
		MaxAttempts:   3,
	})
	if err != nil || !found || newClaim.OperationID != oldClaim.OperationID ||
		newClaim.PriorStatus != operationdomain.StatusRunning || newClaim.AttemptNumber != 2 ||
		newClaim.LeaseEpoch != oldClaim.LeaseEpoch+1 {
		t.Fatalf("过期 running 接管结果错误：old=%+v new=%+v found=%t err=%v", oldClaim, newClaim, found, err)
	}

	oldResult := operationdomain.Resolution{
		OperationID:      oldClaim.OperationID,
		AttemptNumber:    oldClaim.AttemptNumber,
		LeaseOwnerID:     oldClaim.LeaseOwnerID,
		LeaseEpoch:       oldClaim.LeaseEpoch,
		DesiredRevision:  oldClaim.DesiredRevision,
		Status:           operationdomain.StatusUnknown,
		NextAttemptDelay: time.Millisecond,
		ErrorCode:        mailcore.ErrorCodeUnknown,
	}
	if err = repository.ResolveMailboxOperation(ctx, oldResult); !errors.Is(err, operationdomain.ErrLeaseLost) {
		t.Fatalf("接管后的旧 worker 回执必须被 fencing：%v", err)
	}
	if err = repository.ResolveMailboxOperation(ctx, operationdomain.Resolution{
		OperationID:      newClaim.OperationID,
		AttemptNumber:    newClaim.AttemptNumber,
		LeaseOwnerID:     newClaim.LeaseOwnerID,
		LeaseEpoch:       newClaim.LeaseEpoch,
		DesiredRevision:  newClaim.DesiredRevision,
		Status:           operationdomain.StatusUnknown,
		NextAttemptDelay: 20 * time.Millisecond,
		ErrorCode:        mailcore.ErrorCodeUnknown,
	}); err != nil {
		t.Fatalf("提交接管 attempt 的 unknown 回执失败：%v", err)
	}
	if _, found, err = repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
	}); err != nil || found {
		t.Fatalf("退避未到期时不得再次领取：found=%t err=%v", found, err)
	}
	time.Sleep(30 * time.Millisecond)
	thirdClaim, found, err := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
	})
	if err != nil || !found || thirdClaim.OperationID != secondCreated.OperationID ||
		thirdClaim.PriorStatus != operationdomain.StatusUnknown || thirdClaim.AttemptNumber != 3 {
		t.Fatalf("unknown 到期扫描恢复失败：claim=%+v found=%t err=%v", thirdClaim, found, err)
	}
	if err = repository.ResolveMailboxOperation(ctx, operationdomain.Resolution{
		OperationID:     thirdClaim.OperationID,
		AttemptNumber:   thirdClaim.AttemptNumber,
		LeaseOwnerID:    thirdClaim.LeaseOwnerID,
		LeaseEpoch:      thirdClaim.LeaseEpoch,
		DesiredRevision: thirdClaim.DesiredRevision,
		Status:          operationdomain.StatusFailed,
		ErrorCode:       "MAIL_CORE_CONFLICT",
	}); err != nil {
		t.Fatalf("结束恢复测试 operation 失败：%v", err)
	}
	assertTakeoverAttempts(t, ctx, pool, secondCreated.OperationID)

	supersededCreated, err := service.RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "worker-superseded",
		DisplayName:    "Worker Superseded",
		IdempotencyKey: "worker-superseded-request-0001",
	})
	if err != nil {
		t.Fatalf("创建旧 revision 测试 operation 失败：%v", err)
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE mailboxes SET revision = 2 WHERE id = $1",
		supersededCreated.MailboxID,
	); err != nil {
		t.Fatalf("推进 mailbox revision 失败：%v", err)
	}
	if _, found, err = repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
	}); err != nil || found {
		t.Fatalf("旧 revision operation 不得被领取：found=%t err=%v", found, err)
	}
	assertFinalizedOperation(
		t,
		ctx,
		pool,
		supersededCreated.OperationID,
		"superseded",
		0,
		"OPERATION_REVISION_SUPERSEDED",
		0,
	)

	supersededRunningCreated, err := service.RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "worker-superseded-running",
		DisplayName:    "Worker Superseded Running",
		IdempotencyKey: "worker-superseded-running-0001",
	})
	if err != nil {
		t.Fatalf("创建 running 旧 revision 测试 operation 失败：%v", err)
	}
	supersededRunningClaim, found, err := repository.ClaimMailboxOperation(
		ctx,
		operationdomain.ClaimRequest{
			OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
		},
	)
	if err != nil || !found || supersededRunningClaim.OperationID != supersededRunningCreated.OperationID {
		t.Fatalf("领取 running 旧 revision 测试 operation 失败：claim=%+v found=%t err=%v", supersededRunningClaim, found, err)
	}
	if _, err = pool.Exec(
		ctx,
		"UPDATE mailboxes SET revision = 2 WHERE id = $1",
		supersededRunningCreated.MailboxID,
	); err != nil {
		t.Fatalf("推进 running operation 的 mailbox revision 失败：%v", err)
	}
	if _, found, err = repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
	}); err != nil || found {
		t.Fatalf("running 旧 revision operation 不得被再次领取：found=%t err=%v", found, err)
	}
	assertFinalizedOperation(
		t,
		ctx,
		pool,
		supersededRunningCreated.OperationID,
		"superseded",
		1,
		"OPERATION_REVISION_SUPERSEDED",
		1,
	)

	exhaustedCreated, err := service.RequestMailbox(ctx, provisioning.RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "worker-exhausted",
		DisplayName:    "Worker Exhausted",
		IdempotencyKey: "worker-exhausted-request-0001",
	})
	if err != nil {
		t.Fatalf("创建 attempt 耗尽测试 operation 失败：%v", err)
	}
	exhaustedClaim, found, err := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: 5 * time.Millisecond, MaxAttempts: 1,
	})
	if err != nil || !found || exhaustedClaim.OperationID != exhaustedCreated.OperationID ||
		exhaustedClaim.AttemptNumber != 1 {
		t.Fatalf("领取最后一次 attempt 失败：claim=%+v found=%t err=%v", exhaustedClaim, found, err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, found, err = repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 1,
	}); err != nil || found {
		t.Fatalf("耗尽 attempt 后不得创建超上限 attempt：found=%t err=%v", found, err)
	}
	assertFinalizedOperation(
		t,
		ctx,
		pool,
		exhaustedCreated.OperationID,
		"dead",
		1,
		"WORKER_ATTEMPTS_EXHAUSTED",
		1,
	)
}

func TestMailboxOperationMigrationPreservesRetryWaitAcrossDownUp(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过 operation worker migration 往返测试")
	}
	ctx := context.Background()
	isolatedDatabaseURL := isolatedOperationDatabaseURL(t, databaseURL)
	if err := migrations.Run(ctx, isolatedDatabaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("隔离 schema 应用 migration 失败：%v", err)
	}

	pool, err := pgxpool.New(ctx, isolatedDatabaseURL)
	if err != nil {
		t.Fatalf("创建 migration 往返测试连接池失败：%v", err)
	}
	t.Cleanup(pool.Close)
	tenantID := uuid.New()
	otherTenantID := uuid.New()
	domainID := uuid.New()
	insertProvisioningFixtures(
		t,
		ctx,
		pool,
		tenantID,
		otherTenantID,
		domainID,
		uuid.New(),
		uuid.New(),
	)

	repository := NewProvisioningRepository(pool)
	created, err := provisioning.NewService(repository).RequestMailbox(
		ctx,
		provisioning.RequestMailboxCommand{
			TenantID:       tenantID,
			DomainID:       domainID,
			LocalPart:      "migration-retry",
			DisplayName:    "Migration Retry",
			IdempotencyKey: "migration-retry-request-0001",
		},
	)
	if err != nil {
		t.Fatalf("创建 migration 往返测试 operation 失败：%v", err)
	}
	claimed, found, err := repository.ClaimMailboxOperation(ctx, operationdomain.ClaimRequest{
		OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
	})
	if err != nil || !found || claimed.OperationID != created.OperationID {
		t.Fatalf("领取 migration 往返测试 operation 失败：claim=%+v found=%t err=%v", claimed, found, err)
	}
	if err = repository.ResolveMailboxOperation(ctx, operationdomain.Resolution{
		OperationID:      claimed.OperationID,
		AttemptNumber:    claimed.AttemptNumber,
		LeaseOwnerID:     claimed.LeaseOwnerID,
		LeaseEpoch:       claimed.LeaseEpoch,
		DesiredRevision:  claimed.DesiredRevision,
		Status:           operationdomain.StatusRetryWait,
		NextAttemptDelay: time.Hour,
		ErrorCode:        "MAIL_CORE_TEMPORARY",
	}); err != nil {
		t.Fatalf("准备 retry_wait migration 往返状态失败：%v", err)
	}

	if err = migrations.Run(ctx, isolatedDatabaseURL, migrations.CommandDown, io.Discard); err != nil {
		t.Fatalf("回滚 operation worker migration 失败：%v", err)
	}
	assertOperationStatus(t, ctx, pool, created.OperationID, "pending")
	if err = migrations.Run(ctx, isolatedDatabaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("重新应用 operation worker migration 失败：%v", err)
	}
	assertOperationStatus(t, ctx, pool, created.OperationID, "pending")
	claimed, found, err = NewProvisioningRepository(pool).ClaimMailboxOperation(
		ctx,
		operationdomain.ClaimRequest{
			OwnerID: uuid.New(), LeaseDuration: time.Second, MaxAttempts: 3,
		},
	)
	if err != nil || !found || claimed.OperationID != created.OperationID || claimed.AttemptNumber != 1 {
		t.Fatalf("down/up 后 retry_wait 未恢复为可领取任务：claim=%+v found=%t err=%v", claimed, found, err)
	}
}

// assertOperationStatus 核对 migration 往返前后的 operation 状态。
func assertOperationStatus(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	operationID uuid.UUID,
	want string,
) {
	t.Helper()
	var got string
	if err := pool.QueryRow(ctx, "SELECT status FROM operations WHERE id = $1", operationID).Scan(&got); err != nil {
		t.Fatalf("读取 migration 往返 operation 状态失败：%v", err)
	}
	if got != want {
		t.Fatalf("migration 往返 operation 状态错误：got=%s want=%s", got, want)
	}
}

// assertFinalizedOperation 核对 superseded/dead 终态没有创建额外 attempt，并关闭遗留执行记录。
func assertFinalizedOperation(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	operationID uuid.UUID,
	wantStatus string,
	wantAttemptCount int,
	wantErrorCode string,
	wantAttemptRows int,
) {
	t.Helper()
	var status string
	var attemptCount int
	var errorCode string
	var attemptRows int
	var unfinishedAttempts int
	var matchingTerminalAttempts int
	if err := pool.QueryRow(ctx, `
        SELECT
            operations.status,
            operations.attempt_count,
            operations.last_error_code,
            (SELECT count(*) FROM operation_attempts WHERE operation_id = operations.id),
            (SELECT count(*) FROM operation_attempts
                WHERE operation_id = operations.id AND completed_at IS NULL),
            (SELECT count(*) FROM operation_attempts
                WHERE operation_id = operations.id AND result_status = $2)
        FROM operations
        WHERE operations.id = $1
    `, operationID, wantStatus).Scan(
		&status,
		&attemptCount,
		&errorCode,
		&attemptRows,
		&unfinishedAttempts,
		&matchingTerminalAttempts,
	); err != nil {
		t.Fatalf("读取领取前终结结果失败：%v", err)
	}
	if status != wantStatus || attemptCount != wantAttemptCount || errorCode != wantErrorCode ||
		attemptRows != wantAttemptRows || unfinishedAttempts != 0 ||
		matchingTerminalAttempts != wantAttemptRows {
		t.Fatalf(
			"领取前终结结果错误：status=%s attempts=%d error=%s rows=%d unfinished=%d terminal=%d",
			status,
			attemptCount,
			errorCode,
			attemptRows,
			unfinishedAttempts,
			matchingTerminalAttempts,
		)
	}
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
		"DELETE FROM operation_attempts WHERE tenant_id IN ($1, $2)",
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

// assertClaimPersistence 核对领取、attempt 创建和 outbox 唤醒推进处于同一个提交结果中。
func assertClaimPersistence(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	operationID uuid.UUID,
	wantStatus string,
	wantAttemptCount int,
	wantLeaseEpoch int64,
	wantOutboxStatus string,
	wantOutboxAttempts int,
) {
	t.Helper()
	var status string
	var attemptCount int
	var leaseEpoch int64
	var attemptRows int
	var outboxStatus string
	var outboxAttempts int
	if err := pool.QueryRow(ctx, `
        SELECT
            operations.status,
            operations.attempt_count,
            operations.lease_epoch,
            (SELECT count(*) FROM operation_attempts WHERE operation_id = operations.id),
            outbox_events.status,
            outbox_events.attempts
        FROM operations
        JOIN outbox_events ON outbox_events.operation_id = operations.id
        WHERE operations.id = $1
    `, operationID).Scan(
		&status,
		&attemptCount,
		&leaseEpoch,
		&attemptRows,
		&outboxStatus,
		&outboxAttempts,
	); err != nil {
		t.Fatalf("读取 operation 领取持久化结果失败：%v", err)
	}
	if status != wantStatus || attemptCount != wantAttemptCount || attemptRows != wantAttemptCount ||
		leaseEpoch != wantLeaseEpoch || outboxStatus != wantOutboxStatus ||
		outboxAttempts != wantOutboxAttempts {
		t.Fatalf(
			"operation 领取未原子推进：status=%s attempts=%d rows=%d epoch=%d outbox=%s/%d",
			status,
			attemptCount,
			attemptRows,
			leaseEpoch,
			outboxStatus,
			outboxAttempts,
		)
	}
}

// assertSuccessfulOperation 核对成功回执原子完成 attempt、operation 和 mailbox 实际状态。
func assertSuccessfulOperation(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	claimed operationdomain.ClaimedMailboxOperation,
	observedAt time.Time,
) {
	t.Helper()
	var operationStatus string
	var mailboxStatus string
	var operationObservedRevision int64
	var mailboxObservedRevision int64
	var operationHash []byte
	var mailboxHash []byte
	var persistedObservedAt time.Time
	var attemptStatus string
	if err := pool.QueryRow(ctx, `
        SELECT
            operations.status,
            operations.observed_revision,
            operations.observed_configuration_hash,
            mailboxes.observed_status,
            mailboxes.observed_revision,
            mailboxes.observed_configuration_hash,
            mailboxes.observed_at,
            operation_attempts.result_status
        FROM operations
        JOIN mailboxes ON mailboxes.id = operations.resource_id
        JOIN operation_attempts
          ON operation_attempts.operation_id = operations.id
         AND operation_attempts.attempt_number = 1
        WHERE operations.id = $1
    `, claimed.OperationID).Scan(
		&operationStatus,
		&operationObservedRevision,
		&operationHash,
		&mailboxStatus,
		&mailboxObservedRevision,
		&mailboxHash,
		&persistedObservedAt,
		&attemptStatus,
	); err != nil {
		t.Fatalf("读取 operation 成功回执结果失败：%v", err)
	}
	if operationStatus != "succeeded" || mailboxStatus != "active" || attemptStatus != "succeeded" ||
		operationObservedRevision != claimed.DesiredRevision || mailboxObservedRevision != claimed.DesiredRevision ||
		!bytes.Equal(operationHash, claimed.ConfigurationHash[:]) ||
		!bytes.Equal(mailboxHash, claimed.ConfigurationHash[:]) || !persistedObservedAt.Equal(observedAt) {
		t.Fatalf(
			"成功回执未原子收敛：operation=%s mailbox=%s attempt=%s operationRevision=%d mailboxRevision=%d observedAt=%s",
			operationStatus,
			mailboxStatus,
			attemptStatus,
			operationObservedRevision,
			mailboxObservedRevision,
			persistedObservedAt,
		)
	}
}

// assertTakeoverAttempts 核对过期 attempt 被标记 unknown，后续 attempt 按 epoch 单调追加。
func assertTakeoverAttempts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	operationID uuid.UUID,
) {
	t.Helper()
	rows, err := pool.Query(ctx, `
        SELECT attempt_number, lease_epoch, prior_status, result_status, error_code
        FROM operation_attempts
        WHERE operation_id = $1
        ORDER BY attempt_number
    `, operationID)
	if err != nil {
		t.Fatalf("读取接管 attempt 失败：%v", err)
	}
	defer rows.Close()
	type attemptState struct {
		number      int
		epoch       int64
		priorStatus string
		result      string
		errorCode   string
	}
	var attempts []attemptState
	for rows.Next() {
		var attempt attemptState
		if err = rows.Scan(
			&attempt.number,
			&attempt.epoch,
			&attempt.priorStatus,
			&attempt.result,
			&attempt.errorCode,
		); err != nil {
			t.Fatalf("解析接管 attempt 失败：%v", err)
		}
		attempts = append(attempts, attempt)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("遍历接管 attempt 失败：%v", err)
	}
	if len(attempts) != 3 ||
		attempts[0] != (attemptState{number: 1, epoch: 1, priorStatus: "pending", result: "unknown", errorCode: "WORKER_LEASE_EXPIRED"}) ||
		attempts[1] != (attemptState{number: 2, epoch: 2, priorStatus: "running", result: "unknown", errorCode: mailcore.ErrorCodeUnknown}) ||
		attempts[2] != (attemptState{number: 3, epoch: 3, priorStatus: "unknown", result: "failed", errorCode: "MAIL_CORE_CONFLICT"}) {
		t.Fatalf("接管 attempt 历史错误：%+v", attempts)
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
	tables := []string{
		"goose_db_version",
		"tenants",
		"domains",
		"mailboxes",
		"operations",
		"operation_attempts",
		"outbox_events",
	}
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
