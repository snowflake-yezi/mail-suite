// Package postgres 实现控制面业务 repository 的 PostgreSQL 持久化边界。
package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres/generated"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/provisioning"
)

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
