// Package operation 编排跨系统副作用的领取、执行、观测和状态收敛。
package operation

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

var (
	// ErrInvalidConfiguration 表示 worker 的租约或重试参数不能保证安全执行。
	ErrInvalidConfiguration = errors.New("operation worker 配置无效")
	// ErrInvalidClaim 表示 repository 返回了缺少执行身份或租约信息的任务。
	ErrInvalidClaim = errors.New("operation 领取结果无效")
	// ErrLeaseLost 表示当前 worker 的 owner 或 epoch 已经过期，回执不得提交。
	ErrLeaseLost = errors.New("operation lease 已失效")
)

// Status 表示 operation ledger 中由 worker 推进的持久化状态。
type Status string

const (
	// StatusPending 表示 operation 尚未发起外部副作用。
	StatusPending Status = "pending"
	// StatusRunning 表示 operation 已由持有有效 lease 的 worker 领取。
	StatusRunning Status = "running"
	// StatusRetryWait 表示确定未完成的临时失败正在等待退避。
	StatusRetryWait Status = "retry_wait"
	// StatusUnknown 表示副作用可能已完成，下次领取必须优先 inspect。
	StatusUnknown Status = "unknown"
	// StatusSucceeded 表示实际观测已匹配当前期望 revision。
	StatusSucceeded Status = "succeeded"
	// StatusFailed 表示 operation 发生确定性永久失败。
	StatusFailed Status = "failed"
	// StatusDead 表示 operation 达到最大尝试次数，需要人工接管。
	StatusDead Status = "dead"
	// StatusSuperseded 表示 operation 已被更高资源 revision 取代，不得再产生副作用。
	StatusSuperseded Status = "superseded"
)

// ClaimRequest 描述 worker 领取一个到期 operation 所需的租约参数。
type ClaimRequest struct {
	// OwnerID 是当前 worker 进程启动时生成的唯一租约 owner。
	OwnerID uuid.UUID
	// LeaseDuration 是数据库授予本次领取的最大持有时间。
	LeaseDuration time.Duration
	// MaxAttempts 是 operation 进入 dead 前允许创建的最大 attempt 数量。
	MaxAttempts int
}

// ClaimedMailboxOperation 是 repository 原子领取并创建 attempt 后返回的执行快照。
type ClaimedMailboxOperation struct {
	// OperationID 是本次逻辑邮箱状态变更的稳定标识。
	OperationID uuid.UUID
	// TenantID 是 operation 所属租户的隔离边界。
	TenantID uuid.UUID
	// MailboxID 是本次开通的控制面邮箱稳定标识。
	MailboxID uuid.UUID
	// DomainID 是邮箱所属邮件域名的控制面稳定标识。
	DomainID uuid.UUID
	// Address 是 adapter 需要使用的规范化完整地址，不得写入普通日志。
	Address string
	// DesiredRevision 是 operation 创建时固定的邮箱期望版本。
	DesiredRevision int64
	// ConfigurationHash 是 operation 期望配置的 SHA-256 摘要。
	ConfigurationHash [32]byte
	// AttemptNumber 是同一 operation 单调递增且从 1 开始的执行次数。
	AttemptNumber int
	// PriorStatus 是领取前状态，用于 unknown 或过期接管时优先 inspect。
	PriorStatus Status
	// LeaseOwnerID 是本次领取的 worker owner，回执必须原样携带。
	LeaseOwnerID uuid.UUID
	// LeaseEpoch 是本次领取单调递增的 fencing token。
	LeaseEpoch int64
	// LeaseExpiresAt 是数据库授予的租约绝对失效时间。
	LeaseExpiresAt time.Time
}

// Resolution 是 worker 在当前 lease 下提交给 repository 的单次 attempt 结果。
type Resolution struct {
	// OperationID 是被收敛的不可变 operation 标识。
	OperationID uuid.UUID
	// AttemptNumber 是本次需要结束的 attempt 序号。
	AttemptNumber int
	// LeaseOwnerID 是领取该 attempt 的 worker owner。
	LeaseOwnerID uuid.UUID
	// LeaseEpoch 是 repository 必须执行 CAS 校验的 fencing token。
	LeaseEpoch int64
	// DesiredRevision 是本次回执声称已经处理的资源期望版本。
	DesiredRevision int64
	// Status 是本次 attempt 后 operation 应进入的状态。
	Status Status
	// NextAttemptDelay 是 retry_wait 或 unknown 下一次允许领取前的退避时长。
	NextAttemptDelay time.Duration
	// ErrorCode 是可持久化的稳定安全错误码，成功时为空。
	ErrorCode string
	// Observation 是本次 inspect 获得的实际状态证据，未取得证据时为空。
	Observation *mailcore.ObservedMailbox
}

// Repository 定义 worker 对 operation ledger 的原子领取和 fenced 回执能力。
type Repository interface {
	// ClaimMailboxOperation 原子领取一个到期任务、增加 epoch 并创建新的 attempt。
	ClaimMailboxOperation(context.Context, ClaimRequest) (ClaimedMailboxOperation, bool, error)
	// ResolveMailboxOperation 仅在 owner、epoch、attempt 和 desired revision 均匹配时提交结果。
	ResolveMailboxOperation(context.Context, Resolution) error
}

// Config 固定 operation worker 的租约、调用截止和重试边界。
type Config struct {
	// OwnerID 是当前 worker 进程生命周期内唯一的租约 owner。
	OwnerID uuid.UUID
	// LeaseDuration 是数据库 lease 的持有时长。
	LeaseDuration time.Duration
	// AttemptTimeout 是单次 adapter ensure 与 inspect 共享的最大执行时长。
	AttemptTimeout time.Duration
	// LeaseSafetyMargin 为 fenced 回执预留的最短数据库提交时间。
	LeaseSafetyMargin time.Duration
	// BaseRetryDelay 是第一次临时失败或 unknown 后的退避时长。
	BaseRetryDelay time.Duration
	// MaxRetryDelay 是指数退避允许达到的上限。
	MaxRetryDelay time.Duration
	// MaxAttempts 是进入 dead 前允许创建的最大 attempt 数量。
	MaxAttempts int
}

// Service 执行单个邮箱开通 operation，并把状态持久化交给 repository。
type Service struct {
	repository Repository
	adapter    mailcore.Adapter
	config     Config
	now        func() time.Time
}

// NewService 创建 mailbox.provision 单次执行服务。
func NewService(repository Repository, adapter mailcore.Adapter, config Config) (*Service, error) {
	return newService(repository, adapter, config, time.Now)
}

// newService 允许测试固定时钟，同时保持生产构造函数只暴露业务依赖。
func newService(
	repository Repository,
	adapter mailcore.Adapter,
	config Config,
	now func() time.Time,
) (*Service, error) {
	if repository == nil || adapter == nil || now == nil ||
		config.OwnerID == uuid.Nil || config.LeaseDuration <= 0 || config.AttemptTimeout <= 0 ||
		config.LeaseSafetyMargin <= 0 ||
		config.LeaseDuration <= config.LeaseSafetyMargin ||
		config.AttemptTimeout > config.LeaseDuration-config.LeaseSafetyMargin ||
		config.BaseRetryDelay <= 0 || config.MaxRetryDelay < config.BaseRetryDelay ||
		config.MaxAttempts <= 0 || config.MaxAttempts > math.MaxInt32 {
		return nil, ErrInvalidConfiguration
	}
	return &Service{repository: repository, adapter: adapter, config: config, now: now}, nil
}

// ClaimNext 从 PostgreSQL ledger 领取一个到期任务；空队列不是错误。
func (service *Service) ClaimNext(ctx context.Context) (ClaimedMailboxOperation, bool, error) {
	return service.repository.ClaimMailboxOperation(ctx, ClaimRequest{
		OwnerID:       service.config.OwnerID,
		LeaseDuration: service.config.LeaseDuration,
		MaxAttempts:   service.config.MaxAttempts,
	})
}

// Execute 在当前 lease 内 ensure 并 inspect，一个 fenced 回执永远不能覆盖新 owner。
func (service *Service) Execute(ctx context.Context, claimed ClaimedMailboxOperation) error {
	if !service.validClaim(claimed) {
		return ErrInvalidClaim
	}

	now := service.now()
	attemptDeadline := now.Add(service.config.AttemptTimeout + service.config.LeaseSafetyMargin)
	if claimed.LeaseExpiresAt.Before(attemptDeadline) {
		attemptDeadline = claimed.LeaseExpiresAt
	}
	attemptContext, cancelAttempt := context.WithDeadline(ctx, attemptDeadline)
	defer cancelAttempt()
	if effectiveDeadline, ok := attemptContext.Deadline(); ok {
		attemptDeadline = effectiveDeadline
	}

	adapterDeadline := attemptDeadline.Add(-service.config.LeaseSafetyMargin)
	if !adapterDeadline.After(service.now()) {
		return service.resolveUncertain(
			attemptContext,
			claimed,
			mailcore.ErrorCodeUnknown,
			nil,
		)
	}

	adapterContext, cancelAdapter := context.WithDeadline(attemptContext, adapterDeadline)
	defer cancelAdapter()

	if claimed.PriorStatus == StatusUnknown || claimed.PriorStatus == StatusRunning {
		observation, err := service.inspect(adapterContext, claimed, adapterDeadline)
		if err != nil {
			return service.resolveAdapterFailure(
				attemptContext,
				claimed,
				mailcore.ErrorClassUnknown,
				err,
				nil,
			)
		}
		if service.matchesDesired(claimed, observation) {
			return service.resolve(attemptContext, claimed, StatusSucceeded, "", &observation)
		}
		if observation.Revision > claimed.DesiredRevision {
			return service.resolveUncertain(
				attemptContext,
				claimed,
				mailcore.ErrorCodeNewerRevision,
				&observation,
			)
		}
	}

	ensureCommand := mailcore.EnsureMailboxCommand{
		OperationID:       claimed.OperationID,
		MailboxID:         claimed.MailboxID,
		DomainID:          claimed.DomainID,
		Address:           claimed.Address,
		DesiredRevision:   claimed.DesiredRevision,
		ConfigurationHash: claimed.ConfigurationHash,
		PayloadVersion:    mailcore.PayloadVersion,
		Deadline:          adapterDeadline,
	}
	if err := service.adapter.EnsureMailbox(adapterContext, ensureCommand); err != nil {
		class, _ := mailcore.ClassifyError(err)
		return service.resolveAdapterFailure(attemptContext, claimed, class, err, nil)
	}

	observation, err := service.inspect(adapterContext, claimed, adapterDeadline)
	if err != nil {
		return service.resolveAdapterFailure(
			attemptContext,
			claimed,
			mailcore.ErrorClassUnknown,
			err,
			nil,
		)
	}
	if !service.matchesDesired(claimed, observation) {
		errorCode := mailcore.ErrorCodeNotConverged
		if observation.Revision > claimed.DesiredRevision {
			errorCode = mailcore.ErrorCodeNewerRevision
		}
		return service.resolveUncertain(attemptContext, claimed, errorCode, &observation)
	}
	return service.resolve(attemptContext, claimed, StatusSucceeded, "", &observation)
}

// inspect 始终调用 adapter 实际读取邮箱状态，不使用 ensure 返回值推断成功。
func (service *Service) inspect(
	ctx context.Context,
	claimed ClaimedMailboxOperation,
	deadline time.Time,
) (mailcore.ObservedMailbox, error) {
	return service.adapter.InspectMailbox(ctx, mailcore.InspectMailboxQuery{
		MailboxID:         claimed.MailboxID,
		DomainID:          claimed.DomainID,
		Address:           claimed.Address,
		DesiredRevision:   claimed.DesiredRevision,
		ConfigurationHash: claimed.ConfigurationHash,
		Deadline:          deadline,
	})
}

// resolveAdapterFailure 根据 adapter 分类选择确定失败、退避、unknown 或 dead。
func (service *Service) resolveAdapterFailure(
	ctx context.Context,
	claimed ClaimedMailboxOperation,
	class mailcore.ErrorClass,
	err error,
	observation *mailcore.ObservedMailbox,
) error {
	_, errorCode := mailcore.ClassifyError(err)
	if errorCode == "" {
		errorCode = mailcore.ErrorCodeUnknown
	}

	switch class {
	case mailcore.ErrorClassPermanent:
		return service.resolve(ctx, claimed, StatusFailed, errorCode, observation)
	case mailcore.ErrorClassRetryable:
		if claimed.AttemptNumber >= service.config.MaxAttempts {
			return service.resolve(ctx, claimed, StatusDead, errorCode, observation)
		}
		return service.resolve(ctx, claimed, StatusRetryWait, errorCode, observation)
	default:
		if claimed.AttemptNumber >= service.config.MaxAttempts {
			return service.resolve(ctx, claimed, StatusDead, errorCode, observation)
		}
		return service.resolve(ctx, claimed, StatusUnknown, errorCode, observation)
	}
}

// resolveUncertain 在 attempt 预算内保留 unknown，耗尽预算后转入 dead 等待人工接管。
func (service *Service) resolveUncertain(
	ctx context.Context,
	claimed ClaimedMailboxOperation,
	errorCode string,
	observation *mailcore.ObservedMailbox,
) error {
	status := StatusUnknown
	if claimed.AttemptNumber >= service.config.MaxAttempts {
		status = StatusDead
	}
	return service.resolve(ctx, claimed, status, errorCode, observation)
}

// resolve 构造携带当前 fencing token 的回执并交由 repository 原子提交。
func (service *Service) resolve(
	ctx context.Context,
	claimed ClaimedMailboxOperation,
	status Status,
	errorCode string,
	observation *mailcore.ObservedMailbox,
) error {
	nextAttemptDelay := time.Duration(0)
	if status == StatusRetryWait || status == StatusUnknown {
		nextAttemptDelay = service.retryDelay(claimed.AttemptNumber)
	}
	return service.repository.ResolveMailboxOperation(ctx, Resolution{
		OperationID:      claimed.OperationID,
		AttemptNumber:    claimed.AttemptNumber,
		LeaseOwnerID:     claimed.LeaseOwnerID,
		LeaseEpoch:       claimed.LeaseEpoch,
		DesiredRevision:  claimed.DesiredRevision,
		Status:           status,
		NextAttemptDelay: nextAttemptDelay,
		ErrorCode:        errorCode,
		Observation:      observation,
	})
}

// matchesDesired 要求状态、revision 与配置摘要同时匹配，禁止用单次调用成功伪造 active。
func (service *Service) matchesDesired(
	claimed ClaimedMailboxOperation,
	observation mailcore.ObservedMailbox,
) bool {
	return observation.MailboxID == claimed.MailboxID &&
		observation.Status == mailcore.MailboxStatusActive &&
		observation.Revision == claimed.DesiredRevision &&
		observation.ConfigurationHash == claimed.ConfigurationHash &&
		!observation.InspectedAt.IsZero()
}

// retryDelay 返回按 attempt 指数增长且封顶的确定性退避时长。
func (service *Service) retryDelay(attempt int) time.Duration {
	delay := service.config.BaseRetryDelay
	for current := 1; current < attempt && delay < service.config.MaxRetryDelay; current++ {
		if delay > service.config.MaxRetryDelay/2 {
			return service.config.MaxRetryDelay
		}
		delay *= 2
	}
	if delay > service.config.MaxRetryDelay {
		return service.config.MaxRetryDelay
	}
	return delay
}

// validClaim 拒绝缺少稳定 ID、revision、attempt 或有效租约的 repository 结果。
func (service *Service) validClaim(claimed ClaimedMailboxOperation) bool {
	return claimed.OperationID != uuid.Nil && claimed.TenantID != uuid.Nil &&
		claimed.MailboxID != uuid.Nil && claimed.DomainID != uuid.Nil && claimed.Address != "" &&
		claimed.DesiredRevision > 0 && claimed.AttemptNumber > 0 &&
		claimed.AttemptNumber <= service.config.MaxAttempts &&
		claimed.LeaseOwnerID == service.config.OwnerID && claimed.LeaseEpoch > 0 &&
		!claimed.LeaseExpiresAt.IsZero() && claimablePriorStatus(claimed.PriorStatus)
}

// claimablePriorStatus 限制 repository 只能返回允许执行或接管的 ledger 状态。
func claimablePriorStatus(status Status) bool {
	switch status {
	case StatusPending, StatusRunning, StatusRetryWait, StatusUnknown:
		return true
	default:
		return false
	}
}
