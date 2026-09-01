package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres/generated"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/provisioning"
)

func TestIdempotentResultReturnsOriginalIdentifiers(t *testing.T) {
	requestHash := [32]byte{1, 2, 3}
	operation := generated.Operation{
		ID:          uuid.New(),
		ResourceID:  uuid.New(),
		RequestHash: requestHash[:],
	}

	result, err := idempotentResult(operation, requestHash)
	if err != nil {
		t.Fatalf("相同指纹重放失败：%v", err)
	}
	if !result.Replayed || result.OperationID != operation.ID || result.MailboxID != operation.ResourceID {
		t.Fatalf("重放未返回原始标识：%+v", result)
	}
}

func TestIdempotentResultRejectsDifferentFingerprint(t *testing.T) {
	operation := generated.Operation{RequestHash: make([]byte, 32)}
	requestHash := [32]byte{1}

	_, err := idempotentResult(operation, requestHash)
	if !provisioning.HasErrorCode(err, provisioning.ErrorCodeIdempotencyConflict) {
		t.Fatalf("不同指纹应返回 IDEMPOTENCY_CONFLICT，实际为 %v", err)
	}
}

func TestPersistenceErrorPreservesCancellationAndHidesDatabaseError(t *testing.T) {
	if !errors.Is(persistenceError(context.Canceled), context.Canceled) {
		t.Fatal("上下文取消必须原样传播")
	}
	databaseError := &pgconn.PgError{Message: "secret database detail"}
	mapped := persistenceError(databaseError)
	if !provisioning.HasErrorCode(mapped, provisioning.ErrorCodePersistenceUnavailable) {
		t.Fatalf("数据库错误应归一化，实际为 %v", mapped)
	}
	if errors.Is(mapped, databaseError) || mapped.Error() == databaseError.Error() {
		t.Fatal("归一化错误不得暴露底层数据库详情")
	}
}

func TestHasConstraintMatchesOnlyNamedPostgreSQLConstraint(t *testing.T) {
	err := &pgconn.PgError{ConstraintName: "mailboxes_domain_local_part_unique"}
	if !hasConstraint(err, "mailboxes_domain_local_part_unique") {
		t.Fatal("应识别目标唯一约束")
	}
	if hasConstraint(err, "other_constraint") {
		t.Fatal("不得把其他约束映射为邮箱已存在")
	}
}
