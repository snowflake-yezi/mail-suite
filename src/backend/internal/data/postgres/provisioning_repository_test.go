package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres/generated"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
	operationdomain "github.com/snowflake-yezi/mail-suite/src/backend/internal/operation"
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

func TestLeaseDurationMicrosecondsRoundsUpWithoutAcceptingNonPositiveValues(t *testing.T) {
	testCases := []struct {
		name       string
		duration   time.Duration
		want       int64
		wantAccept bool
	}{
		{name: "zero", duration: 0, wantAccept: false},
		{name: "negative", duration: -time.Nanosecond, wantAccept: false},
		{name: "sub microsecond", duration: time.Nanosecond, want: 1, wantAccept: true},
		{name: "exact microsecond", duration: time.Microsecond, want: 1, wantAccept: true},
		{name: "round up", duration: time.Microsecond + time.Nanosecond, want: 2, wantAccept: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got, accepted := leaseDurationMicroseconds(testCase.duration)
			if accepted != testCase.wantAccept || got != testCase.want {
				t.Fatalf("租约微秒转换错误：got=(%d,%t) want=(%d,%t)", got, accepted, testCase.want, testCase.wantAccept)
			}
		})
	}
}

func TestClaimMailboxOperationRejectsMissingAttemptLimitBeforeDatabaseAccess(t *testing.T) {
	repository := NewProvisioningRepository(nil)
	_, _, err := repository.ClaimMailboxOperation(context.Background(), operationdomain.ClaimRequest{
		OwnerID:       uuid.New(),
		LeaseDuration: time.Second,
	})
	if !errors.Is(err, operationdomain.ErrInvalidConfiguration) {
		t.Fatalf("缺少最大 attempt 数必须在数据库访问前拒绝：%v", err)
	}
}

func TestValidMailboxResolutionEnforcesTerminalAndObservationContracts(t *testing.T) {
	now := time.Now().UTC()
	mailboxID := uuid.New()
	base := operationdomain.Resolution{
		OperationID:      uuid.New(),
		AttemptNumber:    1,
		LeaseOwnerID:     uuid.New(),
		LeaseEpoch:       1,
		DesiredRevision:  1,
		Status:           operationdomain.StatusRetryWait,
		NextAttemptDelay: time.Second,
		ErrorCode:        "MAIL_CORE_TEMPORARY",
	}
	success := base
	success.Status = operationdomain.StatusSucceeded
	success.NextAttemptDelay = 0
	success.ErrorCode = ""
	success.Observation = &mailcore.ObservedMailbox{
		MailboxID:         mailboxID,
		Status:            mailcore.MailboxStatusActive,
		Revision:          1,
		ConfigurationHash: [32]byte{1},
		InspectedAt:       now,
	}

	testCases := []struct {
		name       string
		resolution operationdomain.Resolution
		want       bool
	}{
		{name: "retry wait", resolution: base, want: true},
		{name: "verified success", resolution: success, want: true},
		{name: "success without observation", resolution: func() operationdomain.Resolution {
			value := success
			value.Observation = nil
			return value
		}(), want: false},
		{name: "unsafe error code", resolution: func() operationdomain.Resolution {
			value := base
			value.ErrorCode = "endpoint alice@example.test"
			return value
		}(), want: false},
		{name: "retry without delay", resolution: func() operationdomain.Resolution {
			value := base
			value.NextAttemptDelay = 0
			return value
		}(), want: false},
		{name: "unsupported observation", resolution: func() operationdomain.Resolution {
			value := base
			value.Observation = &mailcore.ObservedMailbox{
				MailboxID:   mailboxID,
				Status:      mailcore.MailboxStatus("invalid"),
				Revision:    1,
				InspectedAt: now,
			}
			return value
		}(), want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validMailboxResolution(testCase.resolution); got != testCase.want {
				t.Fatalf("回执校验结果错误：got=%t want=%t", got, testCase.want)
			}
		})
	}
}
