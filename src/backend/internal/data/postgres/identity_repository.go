package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres/generated"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

// IdentityRepository 使用 PostgreSQL 实现一次性登录流程、主体映射、会话和认证审计边界。
type IdentityRepository struct {
	pool          *pgxpool.Pool
	idleTimeout   time.Duration
	touchInterval time.Duration
}

// NewIdentityRepository 创建共享连接池上的身份 repository，并拒绝放宽认证契约的超时配置。
func NewIdentityRepository(
	pool *pgxpool.Pool,
	idleTimeout time.Duration,
	touchInterval time.Duration,
) (*IdentityRepository, error) {
	if pool == nil {
		return nil, errors.New("身份 repository 需要 PostgreSQL 连接池")
	}
	if idleTimeout <= 0 || idleTimeout > identity.DefaultSessionIdleTimeout {
		return nil, errors.New("会话空闲超时必须大于零且不超过 30 分钟")
	}
	if touchInterval <= 0 || touchInterval > idleTimeout {
		return nil, errors.New("会话访问写入间隔必须大于零且不超过空闲超时")
	}
	return &IdentityRepository{
		pool:          pool,
		idleTimeout:   idleTimeout,
		touchInterval: touchInterval,
	}, nil
}

// CreateAuthFlow 保存只含摘要和 verifier 密文的一次性 OIDC 登录流程。
func (repository *IdentityRepository) CreateAuthFlow(
	ctx context.Context,
	flow identity.AuthFlowToCreate,
) error {
	normalizedReturnTo, returnPathErr := identity.NormalizeReturnPath(flow.ReturnTo)
	if flow.ID == uuid.Nil ||
		len(flow.PKCEVerifierCiphertext) < 32 ||
		len(flow.PKCEVerifierCiphertext) > 4096 ||
		flow.EncryptionKeyID == "" ||
		flow.EncryptionKeyID != strings.TrimSpace(flow.EncryptionKeyID) ||
		returnPathErr != nil ||
		normalizedReturnTo != flow.ReturnTo ||
		flow.CreatedAt.IsZero() ||
		!flow.ExpiresAt.After(flow.CreatedAt) ||
		flow.ExpiresAt.After(flow.CreatedAt.Add(identity.DefaultAuthFlowLifetime)) {
		return identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	_, err := generated.New(repository.pool).CreateAuthFlow(ctx, generated.CreateAuthFlowParams{
		ID:                     flow.ID,
		StateDigest:            flow.StateDigest[:],
		BrowserCookieDigest:    flow.BrowserCookieDigest[:],
		NonceDigest:            flow.NonceDigest[:],
		PkceVerifierCiphertext: append([]byte(nil), flow.PKCEVerifierCiphertext...),
		EncryptionKeyID:        flow.EncryptionKeyID,
		ReturnTo:               flow.ReturnTo,
		CreatedAt:              timestamptz(flow.CreatedAt),
		ExpiresAt:              timestamptz(flow.ExpiresAt),
	})
	if err != nil {
		return identityPersistenceError(err)
	}
	return nil
}

// ConsumeAuthFlow 原子标记流程已消费，重复、过期或浏览器不匹配统一返回失效错误。
func (repository *IdentityRepository) ConsumeAuthFlow(
	ctx context.Context,
	stateDigest [32]byte,
	browserCookieDigest [32]byte,
	consumedAt time.Time,
) (identity.ConsumedAuthFlow, error) {
	if consumedAt.IsZero() {
		return identity.ConsumedAuthFlow{}, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	row, err := generated.New(repository.pool).ConsumeAuthFlow(ctx, generated.ConsumeAuthFlowParams{
		StateDigest:         stateDigest[:],
		BrowserCookieDigest: browserCookieDigest[:],
		ConsumedAt:          timestamptz(consumedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ConsumedAuthFlow{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if err != nil {
		return identity.ConsumedAuthFlow{}, identityPersistenceError(err)
	}
	nonceDigest, err := fixedDigest(row.NonceDigest)
	if err != nil {
		return identity.ConsumedAuthFlow{}, err
	}
	return identity.ConsumedAuthFlow{
		ID:                     row.ID,
		NonceDigest:            nonceDigest,
		PKCEVerifierCiphertext: append([]byte(nil), row.PkceVerifierCiphertext...),
		EncryptionKeyID:        row.EncryptionKeyID,
		ReturnTo:               row.ReturnTo,
		ExpiresAt:              row.ExpiresAt.Time,
		ConsumedAt:             row.ConsumedAt.Time,
	}, nil
}

// ResolvePrincipal 将已验证 issuer 与 subject 映射到当前有效且具备产品入口的本地主体。
func (repository *IdentityRepository) ResolvePrincipal(
	ctx context.Context,
	issuer string,
	subject string,
) (identity.Principal, error) {
	if issuer == "" || subject == "" {
		return identity.Principal{}, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	row, err := generated.New(repository.pool).GetActivePrincipalByExternalIdentity(
		ctx,
		generated.GetActivePrincipalByExternalIdentityParams{
			OidcIssuer:  issuer,
			OidcSubject: subject,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Principal{}, identity.NewError(identity.ErrorCodeAuthForbidden)
	}
	if err != nil {
		return identity.Principal{}, identityPersistenceError(err)
	}
	return principalFromRow(
		row.ID,
		row.TenantID,
		row.AccountType,
		row.DisplayName,
		row.MailboxID,
		row.MailboxLocalPart,
		row.MailboxDomain,
	)
}

// CreateSession 保存已完成 OIDC 与本地授权校验的不透明服务端会话。
func (repository *IdentityRepository) CreateSession(
	ctx context.Context,
	session identity.SessionToCreate,
) (identity.CreatedSession, error) {
	return createSession(ctx, generated.New(repository.pool), session)
}

// CreateSessionWithAudit 在一个 PostgreSQL 事务中创建会话与登录成功审计。
func (repository *IdentityRepository) CreateSessionWithAudit(
	ctx context.Context,
	session identity.SessionToCreate,
	event identity.AuditEvent,
) (identity.CreatedSession, error) {
	if event.Action != "login" || event.Result != "succeeded" || event.PrincipalID == nil || *event.PrincipalID != session.PrincipalID {
		return identity.CreatedSession{}, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return identity.CreatedSession{}, identityPersistenceError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := generated.New(tx)
	created, err := createSession(ctx, queries, session)
	if err != nil {
		return identity.CreatedSession{}, err
	}
	if err = createAudit(ctx, queries, event); err != nil {
		return identity.CreatedSession{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return identity.CreatedSession{}, identityPersistenceError(err)
	}
	return created, nil
}

// createSession 校验会话生命周期并使用指定 sqlc 查询器持久化。
func createSession(
	ctx context.Context,
	queries *generated.Queries,
	session identity.SessionToCreate,
) (identity.CreatedSession, error) {
	if session.ID == uuid.Nil ||
		session.PrincipalID == uuid.Nil ||
		session.OIDCIssuer == "" ||
		session.OIDCSubject == "" ||
		session.CreatedAt.IsZero() ||
		!session.IdleExpiresAt.After(session.CreatedAt) ||
		session.IdleExpiresAt.After(session.CreatedAt.Add(identity.DefaultSessionIdleTimeout)) ||
		!session.AbsoluteExpiresAt.After(session.CreatedAt) ||
		session.IdleExpiresAt.After(session.AbsoluteExpiresAt) ||
		session.AbsoluteExpiresAt.After(session.CreatedAt.Add(identity.DefaultSessionAbsoluteTimeout)) {
		return identity.CreatedSession{}, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	row, err := queries.CreateUserSession(ctx, generated.CreateUserSessionParams{
		ID:                session.ID,
		SessionDigest:     session.SessionDigest[:],
		PrincipalID:       session.PrincipalID,
		OidcIssuer:        session.OIDCIssuer,
		OidcSubject:       session.OIDCSubject,
		OidcSessionID:     nullableText(session.OIDCSessionID),
		CsrfDigest:        session.CSRFDigest[:],
		CreatedAt:         timestamptz(session.CreatedAt),
		IdleExpiresAt:     timestamptz(session.IdleExpiresAt),
		AbsoluteExpiresAt: timestamptz(session.AbsoluteExpiresAt),
	})
	if err != nil {
		return identity.CreatedSession{}, identityPersistenceError(err)
	}
	return identity.CreatedSession{
		ID:                row.ID,
		PrincipalID:       row.PrincipalID,
		CreatedAt:         row.CreatedAt.Time,
		IdleExpiresAt:     row.IdleExpiresAt.Time,
		AbsoluteExpiresAt: row.AbsoluteExpiresAt.Time,
	}, nil
}

// ResolveSession 读取当前状态有效的主体与权限，并按限频策略延长空闲过期时间。
func (repository *IdentityRepository) ResolveSession(
	ctx context.Context,
	sessionDigest [32]byte,
	checkedAt time.Time,
) (identity.Session, error) {
	if checkedAt.IsZero() {
		return identity.Session{}, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	queries := generated.New(repository.pool)
	row, err := queries.GetActiveSessionByDigest(ctx, generated.GetActiveSessionByDigestParams{
		SessionDigest: sessionDigest[:],
		CheckedAt:     timestamptz(checkedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Session{}, identity.NewError(identity.ErrorCodeAuthRequired)
	}
	if err != nil {
		return identity.Session{}, identityPersistenceError(err)
	}

	principal, err := principalFromRow(
		row.PrincipalID,
		row.TenantID,
		row.AccountType,
		row.DisplayName,
		row.MailboxID,
		row.MailboxLocalPart,
		row.MailboxDomain,
	)
	if err != nil {
		return identity.Session{}, err
	}
	permissions, err := queries.ListPrincipalPermissions(ctx, row.PrincipalID)
	if err != nil {
		return identity.Session{}, identityPersistenceError(err)
	}
	csrfDigest, err := fixedDigest(row.CsrfDigest)
	if err != nil {
		return identity.Session{}, err
	}
	session := identity.Session{
		ID:                row.SessionID,
		Principal:         principal,
		Permissions:       permissions,
		CreatedAt:         row.CreatedAt.Time,
		LastSeenAt:        row.LastSeenAt.Time,
		IdleExpiresAt:     row.IdleExpiresAt.Time,
		AbsoluteExpiresAt: row.AbsoluteExpiresAt.Time,
		CSRFDigest:        csrfDigest,
	}
	if !checkedAt.Before(session.LastSeenAt.Add(repository.touchInterval)) {
		touched, touchErr := queries.TouchActiveSession(ctx, generated.TouchActiveSessionParams{
			CheckedAt: timestamptz(checkedAt),
			IdleTimeout: pgtype.Interval{
				Microseconds: repository.idleTimeout.Microseconds(),
				Valid:        true,
			},
			SessionID: session.ID,
		})
		if errors.Is(touchErr, pgx.ErrNoRows) {
			return identity.Session{}, identity.NewError(identity.ErrorCodeAuthRequired)
		}
		if touchErr != nil {
			return identity.Session{}, identityPersistenceError(touchErr)
		}
		session.LastSeenAt = touched.LastSeenAt.Time
		session.IdleExpiresAt = touched.IdleExpiresAt.Time
	}
	return session, nil
}

// RevokeSession 按 Cookie 摘要幂等撤销本地会话。
func (repository *IdentityRepository) RevokeSession(
	ctx context.Context,
	sessionDigest [32]byte,
	revokedAt time.Time,
	reason identity.RevocationReason,
) error {
	return revokeSession(ctx, generated.New(repository.pool), sessionDigest, revokedAt, reason)
}

// RevokeSessionWithAudit 在一个 PostgreSQL 事务中撤销会话与记录退出成功审计。
func (repository *IdentityRepository) RevokeSessionWithAudit(
	ctx context.Context,
	sessionDigest [32]byte,
	revokedAt time.Time,
	reason identity.RevocationReason,
	event identity.AuditEvent,
) error {
	if event.Action != "logout" || event.Result != "succeeded" || event.PrincipalID == nil {
		return identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return identityPersistenceError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := generated.New(tx)
	if err = revokeSession(ctx, queries, sessionDigest, revokedAt, reason); err != nil {
		return err
	}
	if err = createAudit(ctx, queries, event); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return identityPersistenceError(err)
	}
	return nil
}

// revokeSession 使用指定 sqlc 查询器幂等撤销会话。
func revokeSession(
	ctx context.Context,
	queries *generated.Queries,
	sessionDigest [32]byte,
	revokedAt time.Time,
	reason identity.RevocationReason,
) error {
	if revokedAt.IsZero() || !reason.Valid() {
		return identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	_, err := queries.RevokeSessionByDigest(ctx, generated.RevokeSessionByDigestParams{
		SessionDigest:    sessionDigest[:],
		RevokedAt:        timestamptz(revokedAt),
		RevocationReason: nullableText(string(reason)),
	})
	if err != nil {
		return identityPersistenceError(err)
	}
	return nil
}

// RevokeByOIDCSessionID 按 issuer 与 sid 幂等撤销匹配的设备会话。
func (repository *IdentityRepository) RevokeByOIDCSessionID(
	ctx context.Context,
	issuer string,
	sessionID string,
	revokedAt time.Time,
) (int64, error) {
	if issuer == "" || sessionID == "" || revokedAt.IsZero() {
		return 0, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	count, err := generated.New(repository.pool).RevokeSessionsByOidcSessionID(
		ctx,
		generated.RevokeSessionsByOidcSessionIDParams{
			OidcIssuer:       issuer,
			OidcSessionID:    nullableText(sessionID),
			RevokedAt:        timestamptz(revokedAt),
			RevocationReason: nullableText(string(identity.RevocationReasonBackchannelSID)),
		},
	)
	if err != nil {
		return 0, identityPersistenceError(err)
	}
	return count, nil
}

// RevokeByOIDCSubject 按 issuer 与 subject 幂等撤销该主体全部设备会话。
func (repository *IdentityRepository) RevokeByOIDCSubject(
	ctx context.Context,
	issuer string,
	subject string,
	revokedAt time.Time,
) (int64, error) {
	if issuer == "" || subject == "" || revokedAt.IsZero() {
		return 0, identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	count, err := generated.New(repository.pool).RevokeSessionsByOidcSubject(
		ctx,
		generated.RevokeSessionsByOidcSubjectParams{
			OidcIssuer:       issuer,
			OidcSubject:      subject,
			RevokedAt:        timestamptz(revokedAt),
			RevocationReason: nullableText(string(identity.RevocationReasonBackchannelSubject)),
		},
	)
	if err != nil {
		return 0, identityPersistenceError(err)
	}
	return count, nil
}

// RecordAudit 保存经过调用方脱敏的认证安全审计事件。
func (repository *IdentityRepository) RecordAudit(ctx context.Context, event identity.AuditEvent) error {
	return createAudit(ctx, generated.New(repository.pool), event)
}

// createAudit 校验并使用指定 sqlc 查询器写入脱敏认证审计。
func createAudit(ctx context.Context, queries *generated.Queries, event identity.AuditEvent) error {
	if event.ID == uuid.Nil ||
		event.Action == "" ||
		(event.Result != "succeeded" && event.Result != "failed") ||
		(event.Result == "succeeded" && event.FailureCategory != "") ||
		(event.Result == "failed" && event.FailureCategory == "") ||
		event.RequestID == "" ||
		event.OccurredAt.IsZero() {
		return identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	err := queries.CreateAuthenticationAuditEvent(
		ctx,
		generated.CreateAuthenticationAuditEventParams{
			ID:              event.ID,
			PrincipalID:     nullableUUID(event.PrincipalID),
			Action:          event.Action,
			Result:          event.Result,
			FailureCategory: nullableText(event.FailureCategory),
			SourceDigest:    event.SourceDigest[:],
			RequestID:       event.RequestID,
			OccurredAt:      timestamptz(event.OccurredAt),
		},
	)
	if err != nil {
		return identityPersistenceError(err)
	}
	return nil
}

// principalFromRow 将数据库可空邮箱字段转换为账号类型约束明确的领域主体。
func principalFromRow(
	principalID uuid.UUID,
	tenantID uuid.UUID,
	accountTypeValue string,
	displayName string,
	mailboxID pgtype.UUID,
	mailboxLocalPart pgtype.Text,
	mailboxDomain pgtype.Text,
) (identity.Principal, error) {
	accountType := identity.AccountType(accountTypeValue)
	if !accountType.Valid() {
		return identity.Principal{}, identity.NewError(identity.ErrorCodeAuthForbidden)
	}
	principal := identity.Principal{
		ID:          principalID,
		TenantID:    tenantID,
		AccountType: accountType,
		DisplayName: displayName,
	}
	if accountType == identity.AccountTypeMailbox {
		if !mailboxID.Valid || !mailboxLocalPart.Valid || !mailboxDomain.Valid {
			return identity.Principal{}, identity.NewError(identity.ErrorCodeAuthForbidden)
		}
		principal.Mailbox = &identity.Mailbox{
			ID:      uuid.UUID(mailboxID.Bytes),
			Address: mailboxLocalPart.String + "@" + mailboxDomain.String,
		}
	} else if mailboxID.Valid || mailboxLocalPart.Valid || mailboxDomain.Valid {
		return identity.Principal{}, identity.NewError(identity.ErrorCodeAuthForbidden)
	}
	return principal, nil
}

// fixedDigest 将数据库字节串转换为固定长度摘要并防御损坏的持久化数据。
func fixedDigest(value []byte) ([32]byte, error) {
	var digest [32]byte
	if len(value) != len(digest) {
		return digest, identity.NewError(identity.ErrorCodePersistenceUnavailable)
	}
	copy(digest[:], value)
	return digest, nil
}

// timestamptz 将业务 UTC 时间转换为 pgx 的明确非空时间值。
func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}

// nullableText 将空字符串转换为 SQL NULL，避免可选字段出现两套空值语义。
func nullableText(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

// nullableUUID 将可选领域 UUID 转为 pgx 可空 UUID。
func nullableUUID(value *uuid.UUID) pgtype.UUID {
	if value == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *value, Valid: true}
}

// identityPersistenceError 保留调用取消语义，其余底层错误统一脱敏为认证服务不可用。
func identityPersistenceError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return identity.NewError(identity.ErrorCodePersistenceUnavailable)
}

var _ identity.Repository = (*IdentityRepository)(nil)
