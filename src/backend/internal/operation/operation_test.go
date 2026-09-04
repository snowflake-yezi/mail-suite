package operation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

// repositoryFake 模拟 operation ledger 的领取与 fenced 回执，不执行数据库 I/O。
type repositoryFake struct {
	mu                sync.Mutex
	claims            []ClaimedMailboxOperation
	resolutions       []Resolution
	rejectLeaseEpochs map[int64]bool
	claimRequests     []ClaimRequest
	claimCalls        int
}

// ClaimMailboxOperation 按测试预设顺序返回任务。
func (repository *repositoryFake) ClaimMailboxOperation(
	_ context.Context,
	request ClaimRequest,
) (ClaimedMailboxOperation, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.claimCalls++
	repository.claimRequests = append(repository.claimRequests, request)
	if len(repository.claims) == 0 {
		return ClaimedMailboxOperation{}, false, nil
	}
	claimed := repository.claims[0]
	repository.claims = repository.claims[1:]
	return claimed, true, nil
}

// claimSnapshot 返回最近一次领取参数，验证重试上限已经下推到持久化边界。
func (repository *repositoryFake) claimSnapshot() []ClaimRequest {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return append([]ClaimRequest(nil), repository.claimRequests...)
}

// ResolveMailboxOperation 模拟 repository 使用 lease epoch 执行 CAS fencing。
func (repository *repositoryFake) ResolveMailboxOperation(_ context.Context, resolution Resolution) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.rejectLeaseEpochs[resolution.LeaseEpoch] {
		return ErrLeaseLost
	}
	repository.resolutions = append(repository.resolutions, resolution)
	return nil
}

// snapshot 返回不会与 runner 并发写冲突的测试状态副本。
func (repository *repositoryFake) snapshot() ([]Resolution, int) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return append([]Resolution(nil), repository.resolutions...), repository.claimCalls
}

// adapterExecutionKey 表示 fake adapter 仅允许按 operation 与 revision 去重的执行身份。
type adapterExecutionKey struct {
	operationID uuid.UUID
	revision    int64
}

// adapterFake 保存测试邮箱实际状态，并模拟结果未知但副作用已经发生的边界。
type adapterFake struct {
	mu                    sync.Mutex
	now                   time.Time
	ensureErrors          []error
	inspectErrors         []error
	inspectOverrides      []mailcore.ObservedMailbox
	applyBeforeFirstError bool
	states                map[uuid.UUID]mailcore.ObservedMailbox
	executions            map[adapterExecutionKey]struct{}
	ensureCalls           []mailcore.EnsureMailboxCommand
	inspectCalls          []mailcore.InspectMailboxQuery
	applyCount            int
}

// EnsureMailbox 按 operation ID 与 revision 去重，并拒绝旧 revision 覆盖较新状态。
func (adapter *adapterFake) EnsureMailbox(
	_ context.Context,
	command mailcore.EnsureMailboxCommand,
) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.ensureCalls = append(adapter.ensureCalls, command)

	var configuredError error
	if len(adapter.ensureErrors) > 0 {
		configuredError = adapter.ensureErrors[0]
		adapter.ensureErrors = adapter.ensureErrors[1:]
	}
	shouldApply := configuredError == nil || (adapter.applyBeforeFirstError && len(adapter.ensureCalls) == 1)
	key := adapterExecutionKey{operationID: command.OperationID, revision: command.DesiredRevision}
	_, replayed := adapter.executions[key]
	current, exists := adapter.states[command.MailboxID]
	if shouldApply && !replayed && (!exists || current.Revision <= command.DesiredRevision) {
		adapter.executions[key] = struct{}{}
		adapter.states[command.MailboxID] = mailcore.ObservedMailbox{
			MailboxID:         command.MailboxID,
			Status:            mailcore.MailboxStatusActive,
			Revision:          command.DesiredRevision,
			ConfigurationHash: command.ConfigurationHash,
			InspectedAt:       adapter.now,
		}
		adapter.applyCount++
	}
	return configuredError
}

// InspectMailbox 始终读取 fake 的当前实际状态或返回测试注入结果。
func (adapter *adapterFake) InspectMailbox(
	_ context.Context,
	query mailcore.InspectMailboxQuery,
) (mailcore.ObservedMailbox, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.inspectCalls = append(adapter.inspectCalls, query)
	if len(adapter.inspectErrors) > 0 {
		err := adapter.inspectErrors[0]
		adapter.inspectErrors = adapter.inspectErrors[1:]
		return mailcore.ObservedMailbox{}, err
	}
	if len(adapter.inspectOverrides) > 0 {
		observation := adapter.inspectOverrides[0]
		adapter.inspectOverrides = adapter.inspectOverrides[1:]
		return observation, nil
	}
	if observed, ok := adapter.states[query.MailboxID]; ok {
		observed.InspectedAt = adapter.now
		return observed, nil
	}
	return mailcore.ObservedMailbox{
		MailboxID:   query.MailboxID,
		Status:      mailcore.MailboxStatusAbsent,
		InspectedAt: adapter.now,
	}, nil
}

// remove 模拟更高 revision 的删除，以验证再次添加不会复用首次成功。
func (adapter *adapterFake) remove(mailboxID uuid.UUID, revision int64) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.states[mailboxID] = mailcore.ObservedMailbox{
		MailboxID:   mailboxID,
		Status:      mailcore.MailboxStatusAbsent,
		Revision:    revision,
		InspectedAt: adapter.now,
	}
}

// counts 返回 adapter 调用与实际 apply 次数的并发安全快照。
func (adapter *adapterFake) counts() (int, int, int) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return len(adapter.ensureCalls), len(adapter.inspectCalls), adapter.applyCount
}

// commands 返回 adapter 收到的命令副本，供执行身份契约断言使用。
func (adapter *adapterFake) commands() []mailcore.EnsureMailboxCommand {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return append([]mailcore.EnsureMailboxCommand(nil), adapter.ensureCalls...)
}

func TestExecuteSucceedsOnlyAfterMatchingInspect(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)

	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("执行邮箱开通失败：%v", err)
	}
	resolutions, _ := repository.snapshot()
	if len(resolutions) != 1 || resolutions[0].Status != StatusSucceeded ||
		resolutions[0].Observation == nil || resolutions[0].ErrorCode != "" {
		t.Fatalf("ensure + inspect 后回执不正确：%+v", resolutions)
	}
	ensureCalls, inspectCalls, applyCount := adapter.counts()
	if ensureCalls != 1 || inspectCalls != 1 || applyCount != 1 {
		t.Fatalf("必须执行一次 ensure 和一次实际 inspect：ensure=%d inspect=%d apply=%d", ensureCalls, inspectCalls, applyCount)
	}
	commands := adapter.commands()
	if commands[0].OperationID != claimed.OperationID ||
		commands[0].DesiredRevision != claimed.DesiredRevision ||
		commands[0].PayloadVersion != mailcore.PayloadVersion ||
		!commands[0].Deadline.Before(claimed.LeaseExpiresAt) {
		t.Fatalf("adapter 命令缺少稳定执行身份或安全 deadline：%+v", commands[0])
	}
}

func TestUnknownOperationInspectsBeforeReplayingEnsure(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	adapter.applyBeforeFirstError = true
	adapter.ensureErrors = []error{
		mailcore.NewError(mailcore.ErrorClassUnknown, "MAIL_CORE_RESPONSE_LOST"),
	}

	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("提交 unknown 回执失败：%v", err)
	}
	claimed.AttemptNumber = 2
	claimed.PriorStatus = StatusUnknown
	claimed.LeaseEpoch = 2
	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("unknown 接管 inspect 收敛失败：%v", err)
	}

	resolutions, _ := repository.snapshot()
	if len(resolutions) != 2 || resolutions[0].Status != StatusUnknown ||
		resolutions[1].Status != StatusSucceeded {
		t.Fatalf("unknown 状态迁移不正确：%+v", resolutions)
	}
	ensureCalls, inspectCalls, applyCount := adapter.counts()
	if ensureCalls != 1 || inspectCalls != 1 || applyCount != 1 {
		t.Fatalf("接管者必须先 inspect 且不得重复创建：ensure=%d inspect=%d apply=%d", ensureCalls, inspectCalls, applyCount)
	}
}

func TestExecutePropagatesLeaseFencingWithoutPersistingOldResult(t *testing.T) {
	service, repository, _, claimed := newOperationTestFixture(t)
	repository.rejectLeaseEpochs = map[int64]bool{claimed.LeaseEpoch: true}

	err := service.Execute(context.Background(), claimed)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("旧 epoch 回执必须被 fencing：%v", err)
	}
	resolutions, _ := repository.snapshot()
	if len(resolutions) != 0 {
		t.Fatalf("旧 epoch 不得覆盖 ledger：%+v", resolutions)
	}
}

func TestExecuteClassifiesFailureAndCapsRetry(t *testing.T) {
	testCases := []struct {
		name       string
		adapterErr error
		attempt    int
		wantStatus Status
		wantDelay  time.Duration
	}{
		{
			name:       "retryable",
			adapterErr: mailcore.NewError(mailcore.ErrorClassRetryable, "MAIL_CORE_TEMPORARY"),
			attempt:    1,
			wantStatus: StatusRetryWait,
			wantDelay:  time.Second,
		},
		{
			name:       "permanent",
			adapterErr: mailcore.NewError(mailcore.ErrorClassPermanent, "MAIL_CORE_CONFLICT"),
			attempt:    1,
			wantStatus: StatusFailed,
		},
		{
			name:       "attempts exhausted",
			adapterErr: mailcore.NewError(mailcore.ErrorClassRetryable, "MAIL_CORE_TEMPORARY"),
			attempt:    3,
			wantStatus: StatusDead,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service, repository, adapter, claimed := newOperationTestFixture(t)
			claimed.AttemptNumber = testCase.attempt
			adapter.ensureErrors = []error{testCase.adapterErr}
			if err := service.Execute(context.Background(), claimed); err != nil {
				t.Fatalf("提交失败分类回执失败：%v", err)
			}
			resolutions, _ := repository.snapshot()
			if len(resolutions) != 1 || resolutions[0].Status != testCase.wantStatus ||
				resolutions[0].NextAttemptDelay != testCase.wantDelay {
				t.Fatalf("失败分类不正确：%+v", resolutions)
			}
		})
	}
}

func TestExecuteCapsNonConvergedAndNewerObservations(t *testing.T) {
	testCases := []struct {
		name     string
		revision int64
		wantCode string
	}{
		{name: "not converged", revision: 0, wantCode: mailcore.ErrorCodeNotConverged},
		{name: "newer revision", revision: 2, wantCode: mailcore.ErrorCodeNewerRevision},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service, repository, adapter, claimed := newOperationTestFixture(t)
			claimed.AttemptNumber = 3
			adapter.inspectOverrides = []mailcore.ObservedMailbox{{
				MailboxID:         claimed.MailboxID,
				Status:            mailcore.MailboxStatusActive,
				Revision:          testCase.revision,
				ConfigurationHash: claimed.ConfigurationHash,
				InspectedAt:       adapter.now,
			}}

			if err := service.Execute(context.Background(), claimed); err != nil {
				t.Fatalf("提交最后一次未收敛回执失败：%v", err)
			}
			resolutions, _ := repository.snapshot()
			if len(resolutions) != 1 || resolutions[0].Status != StatusDead ||
				resolutions[0].NextAttemptDelay != 0 || resolutions[0].ErrorCode != testCase.wantCode {
				t.Fatalf("最后一次未收敛必须进入 dead：%+v", resolutions)
			}
		})
	}
}

func TestExecuteMarksLastAttemptDeadWhenAdapterCommitWindowIsGone(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	claimed.AttemptNumber = 3
	claimed.LeaseExpiresAt = adapter.now.Add(time.Second)

	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("提交窗口耗尽回执失败：%v", err)
	}
	resolutions, _ := repository.snapshot()
	ensureCalls, inspectCalls, _ := adapter.counts()
	if len(resolutions) != 1 || resolutions[0].Status != StatusDead ||
		resolutions[0].ErrorCode != mailcore.ErrorCodeUnknown || ensureCalls != 0 || inspectCalls != 0 {
		t.Fatalf("最后一次提交窗口耗尽必须无 adapter 调用并进入 dead：resolutions=%+v ensure=%d inspect=%d", resolutions, ensureCalls, inspectCalls)
	}
}

func TestClaimNextPassesAttemptLimitToRepository(t *testing.T) {
	service, repository, _, _ := newOperationTestFixture(t)

	if _, found, err := service.ClaimNext(context.Background()); err != nil || found {
		t.Fatalf("空队列领取失败：found=%t err=%v", found, err)
	}
	requests := repository.claimSnapshot()
	if len(requests) != 1 || requests[0].MaxAttempts != 3 {
		t.Fatalf("领取请求未携带最大 attempt 数：%+v", requests)
	}
}

func TestExecuteKeepsOperationUnknownWhenInspectDoesNotMatch(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	adapter.inspectOverrides = []mailcore.ObservedMailbox{{
		MailboxID:         claimed.MailboxID,
		Status:            mailcore.MailboxStatusActive,
		Revision:          claimed.DesiredRevision - 1,
		ConfigurationHash: claimed.ConfigurationHash,
		InspectedAt:       adapter.now,
	}}

	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("提交未收敛回执失败：%v", err)
	}
	resolutions, _ := repository.snapshot()
	if len(resolutions) != 1 || resolutions[0].Status != StatusUnknown ||
		resolutions[0].ErrorCode != mailcore.ErrorCodeNotConverged {
		t.Fatalf("不匹配 inspect 不得伪装成功：%+v", resolutions)
	}
}

func TestExecuteKeepsOperationUnknownWhenInspectFailsAfterEnsure(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	adapter.inspectErrors = []error{
		mailcore.NewError(mailcore.ErrorClassRetryable, "MAIL_CORE_INSPECT_TIMEOUT"),
	}

	if err := service.Execute(context.Background(), claimed); err != nil {
		t.Fatalf("提交 inspect 失败回执失败：%v", err)
	}
	resolutions, _ := repository.snapshot()
	if len(resolutions) != 1 || resolutions[0].Status != StatusUnknown ||
		resolutions[0].ErrorCode != "MAIL_CORE_INSPECT_TIMEOUT" {
		t.Fatalf("ensure 后 inspect 失败必须保持 unknown：%+v", resolutions)
	}
	_, _, applyCount := adapter.counts()
	if applyCount != 1 {
		t.Fatalf("unknown 必须保留副作用可能已经完成的事实：apply=%d", applyCount)
	}
}

func TestExecuteRejectsTerminalClaimBeforeCallingAdapter(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	claimed.PriorStatus = StatusSucceeded

	if err := service.Execute(context.Background(), claimed); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("终态 operation 不得再次执行：%v", err)
	}
	resolutions, _ := repository.snapshot()
	ensureCalls, inspectCalls, _ := adapter.counts()
	if len(resolutions) != 0 || ensureCalls != 0 || inspectCalls != 0 {
		t.Fatalf("无效领取不得产生调用或回执：resolutions=%d ensure=%d inspect=%d", len(resolutions), ensureCalls, inspectCalls)
	}
}

func TestExecuteRejectsAttemptAboveConfiguredLimitBeforeCallingAdapter(t *testing.T) {
	service, repository, adapter, claimed := newOperationTestFixture(t)
	claimed.AttemptNumber = 4

	if err := service.Execute(context.Background(), claimed); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("超出上限的 attempt 不得执行：%v", err)
	}
	resolutions, _ := repository.snapshot()
	ensureCalls, inspectCalls, _ := adapter.counts()
	if len(resolutions) != 0 || ensureCalls != 0 || inspectCalls != 0 {
		t.Fatalf("超上限领取不得产生调用或回执：resolutions=%d ensure=%d inspect=%d", len(resolutions), ensureCalls, inspectCalls)
	}
}

func TestNewServiceRejectsAttemptWithoutLeaseCommitWindow(t *testing.T) {
	_, repository, adapter, claimed := newOperationTestFixture(t)
	_, err := NewService(repository, adapter, Config{
		OwnerID:           claimed.LeaseOwnerID,
		LeaseDuration:     5 * time.Second,
		AttemptTimeout:    5 * time.Second,
		LeaseSafetyMargin: time.Second,
		BaseRetryDelay:    time.Second,
		MaxRetryDelay:     8 * time.Second,
		MaxAttempts:       3,
	})
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("没有 fenced 回执窗口的配置必须被拒绝：%v", err)
	}
}

func TestAdapterIdentityDoesNotReuseFirstEnsureAfterRemoveAndEnsure(t *testing.T) {
	now := time.Now().UTC()
	adapter := &adapterFake{
		now:        now,
		states:     make(map[uuid.UUID]mailcore.ObservedMailbox),
		executions: make(map[adapterExecutionKey]struct{}),
	}
	mailboxID := uuid.New()
	domainID := uuid.New()
	configurationHash := [32]byte{1, 2, 3}
	first := mailcore.EnsureMailboxCommand{
		OperationID:       uuid.New(),
		MailboxID:         mailboxID,
		DomainID:          domainID,
		Address:           "alice@example.test",
		DesiredRevision:   1,
		ConfigurationHash: configurationHash,
		PayloadVersion:    mailcore.PayloadVersion,
		Deadline:          now.Add(time.Second),
	}
	if err := adapter.EnsureMailbox(context.Background(), first); err != nil {
		t.Fatalf("首次 ensure 失败：%v", err)
	}
	if err := adapter.EnsureMailbox(context.Background(), first); err != nil {
		t.Fatalf("同 operation 重放失败：%v", err)
	}
	adapter.remove(mailboxID, 2)
	third := first
	third.OperationID = uuid.New()
	third.DesiredRevision = 3
	if err := adapter.EnsureMailbox(context.Background(), third); err != nil {
		t.Fatalf("删除后再次 ensure 失败：%v", err)
	}
	if err := adapter.EnsureMailbox(context.Background(), first); err != nil {
		t.Fatalf("历史 operation 重放应安全返回：%v", err)
	}

	observed, err := adapter.InspectMailbox(context.Background(), mailcore.InspectMailboxQuery{
		MailboxID: mailboxID,
	})
	if err != nil {
		t.Fatalf("读取 fake 实际状态失败：%v", err)
	}
	ensureCalls, _, applyCount := adapter.counts()
	if ensureCalls != 4 || applyCount != 2 || observed.Revision != 3 ||
		observed.Status != mailcore.MailboxStatusActive {
		t.Fatalf("apply/remove/apply 被历史成功短路：ensure=%d apply=%d observed=%+v", ensureCalls, applyCount, observed)
	}
}

// newOperationTestFixture 创建使用固定时间和保留域名的隔离 operation 测试对象。
func newOperationTestFixture(
	t *testing.T,
) (*Service, *repositoryFake, *adapterFake, ClaimedMailboxOperation) {
	t.Helper()
	now := time.Now().UTC()
	ownerID := uuid.New()
	claimed := ClaimedMailboxOperation{
		OperationID:       uuid.New(),
		TenantID:          uuid.New(),
		MailboxID:         uuid.New(),
		DomainID:          uuid.New(),
		Address:           "alice@example.test",
		DesiredRevision:   1,
		ConfigurationHash: [32]byte{1, 2, 3},
		AttemptNumber:     1,
		PriorStatus:       StatusPending,
		LeaseOwnerID:      ownerID,
		LeaseEpoch:        1,
		LeaseExpiresAt:    now.Add(10 * time.Second),
	}
	repository := &repositoryFake{rejectLeaseEpochs: make(map[int64]bool)}
	adapter := &adapterFake{
		now:        now,
		states:     make(map[uuid.UUID]mailcore.ObservedMailbox),
		executions: make(map[adapterExecutionKey]struct{}),
	}
	service, err := newService(repository, adapter, Config{
		OwnerID:           ownerID,
		LeaseDuration:     10 * time.Second,
		AttemptTimeout:    5 * time.Second,
		LeaseSafetyMargin: time.Second,
		BaseRetryDelay:    time.Second,
		MaxRetryDelay:     8 * time.Second,
		MaxAttempts:       3,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("创建 operation service 失败：%v", err)
	}
	return service, repository, adapter, claimed
}
