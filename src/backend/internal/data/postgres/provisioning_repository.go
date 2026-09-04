// Package postgres 实现控制面业务 repository 的 PostgreSQL 持久化边界。
package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres/generated"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
	operationdomain "github.com/snowflake-yezi/mail-suite/src/backend/internal/operation"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/provisioning"
)

// operationErrorCodePattern 限制 ledger 只接收不会泄露地址或底层响应的稳定错误码。
var operationErrorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// ProvisioningRepository 使用显式 PostgreSQL 事务保存邮箱意图、operation 和 outbox。
type ProvisioningRepository struct {
	pool *pgxpool.Pool
}

// NewProvisioningRepository 创建共享连接池上的邮箱开通 repository。
func NewProvisioningRepository(pool *pgxpool.Pool) *ProvisioningRepository {
	return &ProvisioningRepository{pool: pool}
}

// RequestMailbox 原子创建邮箱期望状态、operation 和首个 outbox 事件。
func (repository *ProvisioningRepository) RequestMailbox(
	ctx context.Context,
	request provisioning.PreparedRequest,
) (provisioning.RequestMailboxResult, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := generated.New(tx)

	if existing, lookupErr := queries.GetOperationByIdempotencyKey(
		ctx,
		generated.GetOperationByIdempotencyKeyParams{
			TenantID:       request.TenantID,
			IdempotencyKey: request.IdempotencyKey,
		},
	); lookupErr == nil {
		return idempotentResult(existing, request.RequestHash)
	} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return provisioning.RequestMailboxResult{}, persistenceError(lookupErr)
	}

	if _, err = queries.GetProvisionableDomain(
		ctx,
		generated.GetProvisionableDomainParams{ID: request.DomainID, TenantID: request.TenantID},
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return provisioning.RequestMailboxResult{}, provisioning.NewError(provisioning.ErrorCodeDomainUnavailable)
		}
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}

	mailboxID := uuid.New()
	operationID := uuid.New()
	operation, err := queries.InsertOperation(ctx, generated.InsertOperationParams{
		ID:             operationID,
		TenantID:       request.TenantID,
		ResourceID:     mailboxID,
		IdempotencyKey: request.IdempotencyKey,
		RequestHash:    request.RequestHash[:],
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := queries.GetOperationByIdempotencyKey(
			ctx,
			generated.GetOperationByIdempotencyKeyParams{
				TenantID:       request.TenantID,
				IdempotencyKey: request.IdempotencyKey,
			},
		)
		if lookupErr != nil {
			return provisioning.RequestMailboxResult{}, persistenceError(lookupErr)
		}
		return idempotentResult(existing, request.RequestHash)
	}
	if err != nil {
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}

	displayName := pgtype.Text{}
	if request.DisplayName != "" {
		displayName = pgtype.Text{String: request.DisplayName, Valid: true}
	}
	if _, err = queries.InsertMailbox(ctx, generated.InsertMailboxParams{
		ID:          mailboxID,
		TenantID:    request.TenantID,
		DomainID:    request.DomainID,
		LocalPart:   request.LocalPart,
		DisplayName: displayName,
	}); err != nil {
		if hasConstraint(err, "mailboxes_domain_local_part_unique") {
			return provisioning.RequestMailboxResult{}, provisioning.NewError(provisioning.ErrorCodeMailboxAlreadyExists)
		}
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}

	payload, err := json.Marshal(struct {
		TenantID    uuid.UUID `json:"tenant_id"`
		MailboxID   uuid.UUID `json:"mailbox_id"`
		DomainID    uuid.UUID `json:"domain_id"`
		OperationID uuid.UUID `json:"operation_id"`
		Revision    int64     `json:"revision"`
	}{
		TenantID:    request.TenantID,
		MailboxID:   mailboxID,
		DomainID:    request.DomainID,
		OperationID: operationID,
		Revision:    1,
	})
	if err != nil {
		return provisioning.RequestMailboxResult{}, provisioning.NewError(provisioning.ErrorCodePersistenceUnavailable)
	}
	if err = queries.InsertOutboxEvent(ctx, generated.InsertOutboxEventParams{
		ID:          uuid.New(),
		TenantID:    request.TenantID,
		OperationID: operation.ID,
		Payload:     payload,
	}); err != nil {
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}

	if err = tx.Commit(ctx); err != nil {
		return provisioning.RequestMailboxResult{}, persistenceError(err)
	}
	return provisioning.RequestMailboxResult{
		MailboxID:   mailboxID,
		OperationID: operationID,
		Replayed:    false,
	}, nil
}

// GetOperation 通过 tenant 与 operation 复合条件执行不可探测的租户隔离查询。
func (repository *ProvisioningRepository) GetOperation(
	ctx context.Context,
	tenantID uuid.UUID,
	operationID uuid.UUID,
) (provisioning.Operation, error) {
	operation, err := generated.New(repository.pool).GetOperationForTenant(
		ctx,
		generated.GetOperationForTenantParams{TenantID: tenantID, ID: operationID},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return provisioning.Operation{}, provisioning.NewError(provisioning.ErrorCodeOperationNotFound)
	}
	if err != nil {
		return provisioning.Operation{}, persistenceError(err)
	}
	return toDomainOperation(operation), nil
}

// ClaimMailboxOperation 使用 PostgreSQL 行锁领取一个到期邮箱任务，并原子创建不可复用的 attempt。
func (repository *ProvisioningRepository) ClaimMailboxOperation(
	ctx context.Context,
	request operationdomain.ClaimRequest,
) (operationdomain.ClaimedMailboxOperation, bool, error) {
	leaseMicroseconds, ok := leaseDurationMicroseconds(request.LeaseDuration)
	if request.OwnerID == uuid.Nil || !ok || request.MaxAttempts <= 0 ||
		request.MaxAttempts > math.MaxInt32 {
		return operationdomain.ClaimedMailboxOperation{}, false, operationdomain.ErrInvalidConfiguration
	}

	queries := generated.New(repository.pool)
	if _, err := queries.FinalizeMailboxOperationBeforeClaim(
		ctx,
		int32(request.MaxAttempts),
	); err != nil {
		return operationdomain.ClaimedMailboxOperation{}, false, operationPersistenceError(err)
	}

	row, err := queries.ClaimMailboxOperation(
		ctx,
		generated.ClaimMailboxOperationParams{
			OwnerID:                   request.OwnerID,
			LeaseDurationMicroseconds: leaseMicroseconds,
			MaxAttempts:               int32(request.MaxAttempts),
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return operationdomain.ClaimedMailboxOperation{}, false, nil
	}
	if err != nil {
		return operationdomain.ClaimedMailboxOperation{}, false, operationPersistenceError(err)
	}
	if len(row.ConfigurationHash) != 32 || !row.LeaseExpiresAt.Valid {
		return operationdomain.ClaimedMailboxOperation{}, false, operationdomain.ErrInvalidClaim
	}

	var configurationHash [32]byte
	copy(configurationHash[:], row.ConfigurationHash)
	return operationdomain.ClaimedMailboxOperation{
		OperationID:       row.OperationID,
		TenantID:          row.TenantID,
		MailboxID:         row.MailboxID,
		DomainID:          row.DomainID,
		Address:           row.Address,
		DesiredRevision:   row.DesiredRevision,
		ConfigurationHash: configurationHash,
		AttemptNumber:     int(row.AttemptNumber),
		PriorStatus:       operationdomain.Status(row.PriorStatus),
		LeaseOwnerID:      row.LeaseOwnerID,
		LeaseEpoch:        row.LeaseEpoch,
		LeaseExpiresAt:    row.LeaseExpiresAt.Time,
	}, true, nil
}

// ResolveMailboxOperation 在显式事务内提交当前 attempt；fencing 失败时回滚全部状态变化。
func (repository *ProvisioningRepository) ResolveMailboxOperation(
	ctx context.Context,
	resolution operationdomain.Resolution,
) error {
	if !validMailboxResolution(resolution) {
		return operationdomain.ErrInvalidClaim
	}

	observedMailboxID := uuid.Nil
	observedStatus := ""
	observedRevision := int64(0)
	observedConfigurationHash := make([]byte, 0)
	observedAt := time.Unix(0, 0).UTC()
	hasObservation := resolution.Observation != nil
	if hasObservation {
		observedMailboxID = resolution.Observation.MailboxID
		observedStatus = string(resolution.Observation.Status)
		observedRevision = resolution.Observation.Revision
		observedConfigurationHash = resolution.Observation.ConfigurationHash[:]
		observedAt = resolution.Observation.InspectedAt
	}

	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return operationPersistenceError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	applied, err := generated.New(tx).ResolveMailboxOperation(
		ctx,
		generated.ResolveMailboxOperationParams{
			OperationID:                  resolution.OperationID,
			LeaseOwnerID:                 resolution.LeaseOwnerID,
			LeaseEpoch:                   resolution.LeaseEpoch,
			AttemptNumber:                int32(resolution.AttemptNumber),
			DesiredRevision:              resolution.DesiredRevision,
			ResultStatus:                 string(resolution.Status),
			HasObservation:               hasObservation,
			ObservedMailboxID:            observedMailboxID,
			ObservedStatus:               observedStatus,
			ObservedRevision:             observedRevision,
			ObservedConfigurationHash:    observedConfigurationHash,
			ObservedAt:                   pgtype.Timestamptz{Time: observedAt, Valid: true},
			NextAttemptDelayMicroseconds: durationMicroseconds(resolution.NextAttemptDelay),
			ErrorCode:                    resolution.ErrorCode,
		},
	)
	if err != nil {
		return operationPersistenceError(err)
	}
	if !applied.Valid || !applied.Bool {
		return operationdomain.ErrLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return operationPersistenceError(err)
	}
	return nil
}

// validMailboxResolution 拒绝不完整结果，避免绕过领域服务污染 operation ledger。
func validMailboxResolution(resolution operationdomain.Resolution) bool {
	if resolution.OperationID == uuid.Nil || resolution.LeaseOwnerID == uuid.Nil ||
		resolution.LeaseEpoch <= 0 || resolution.AttemptNumber <= 0 ||
		resolution.AttemptNumber > math.MaxInt32 || resolution.DesiredRevision <= 0 {
		return false
	}

	terminal := resolution.Status == operationdomain.StatusSucceeded ||
		resolution.Status == operationdomain.StatusFailed || resolution.Status == operationdomain.StatusDead
	retrying := resolution.Status == operationdomain.StatusRetryWait ||
		resolution.Status == operationdomain.StatusUnknown
	if !terminal && !retrying {
		return false
	}
	if retrying != (resolution.NextAttemptDelay > 0) ||
		terminal && resolution.NextAttemptDelay != 0 {
		return false
	}
	if resolution.Status == operationdomain.StatusSucceeded {
		if resolution.ErrorCode != "" || resolution.Observation == nil {
			return false
		}
	} else if !operationErrorCodePattern.MatchString(resolution.ErrorCode) {
		return false
	}
	if resolution.Observation == nil {
		return true
	}
	return resolution.Observation.MailboxID != uuid.Nil &&
		validObservedMailboxStatus(resolution.Observation.Status) &&
		resolution.Observation.Revision >= 0 && !resolution.Observation.InspectedAt.IsZero()
}

// validObservedMailboxStatus 限制数据库只保存 adapter 契约定义的实际邮箱状态。
func validObservedMailboxStatus(status mailcore.MailboxStatus) bool {
	switch status {
	case mailcore.MailboxStatusAbsent, mailcore.MailboxStatusActive, mailcore.MailboxStatusSuspended:
		return true
	default:
		return false
	}
}

// leaseDurationMicroseconds 将正租约向上取整到 PostgreSQL 支持的微秒精度。
func leaseDurationMicroseconds(duration time.Duration) (int64, bool) {
	if duration <= 0 {
		return 0, false
	}
	microseconds := int64(duration / time.Microsecond)
	if duration%time.Microsecond != 0 {
		microseconds++
	}
	return microseconds, microseconds > 0
}

// durationMicroseconds 将已校验的非负退避时长转换为 PostgreSQL interval 参数。
func durationMicroseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	microseconds, _ := leaseDurationMicroseconds(duration)
	return microseconds
}

// operationPersistenceError 保留取消语义，并隐藏 SQL、地址和数据库拓扑。
func operationPersistenceError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("operation 持久化服务暂不可用")
}

// idempotentResult 比较持久化指纹，并返回原始资源标识或稳定冲突。
func idempotentResult(
	operation generated.Operation,
	requestHash [32]byte,
) (provisioning.RequestMailboxResult, error) {
	if !bytes.Equal(operation.RequestHash, requestHash[:]) {
		return provisioning.RequestMailboxResult{}, provisioning.NewError(provisioning.ErrorCodeIdempotencyConflict)
	}
	return provisioning.RequestMailboxResult{
		MailboxID:   operation.ResourceID,
		OperationID: operation.ID,
		Replayed:    true,
	}, nil
}

// toDomainOperation 删除数据库专用字段，只向领域层暴露业务状态。
func toDomainOperation(operation generated.Operation) provisioning.Operation {
	return provisioning.Operation{
		ID:           operation.ID,
		TenantID:     operation.TenantID,
		Kind:         operation.Kind,
		ResourceType: operation.ResourceType,
		ResourceID:   operation.ResourceID,
		Status:       operation.Status,
	}
}

// hasConstraint 使用 PostgreSQL 约束名识别可公开的确定性业务冲突。
func hasConstraint(err error, constraintName string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.ConstraintName == constraintName
}

// persistenceError 保留调用取消语义，其余底层错误统一脱敏。
func persistenceError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return provisioning.NewError(provisioning.ErrorCodePersistenceUnavailable)
}
