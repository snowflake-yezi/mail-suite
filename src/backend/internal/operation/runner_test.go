package operation

import (
	"context"
	"sync"
	"testing"
	"time"
)

// executorFake 协调 runner 取消与当前 attempt 收尾的并发测试。
type executorFake struct {
	mu           sync.Mutex
	claim        ClaimedMailboxOperation
	hasClaim     bool
	claimCalls   int
	executeCalls int
	entered      chan struct{}
	release      chan struct{}
}

// deadlineRepository 模拟数据库回执阻塞，直到 attempt 自身 deadline 取消查询。
type deadlineRepository struct {
	mu             sync.Mutex
	claim          ClaimedMailboxOperation
	claimed        bool
	resolveEntered chan struct{}
}

// ClaimMailboxOperation 只返回一次任务，供 runner 进入真实 Service 执行路径。
func (repository *deadlineRepository) ClaimMailboxOperation(
	_ context.Context,
	_ ClaimRequest,
) (ClaimedMailboxOperation, bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.claimed {
		return ClaimedMailboxOperation{}, false, nil
	}
	repository.claimed = true
	return repository.claim, true, nil
}

// ResolveMailboxOperation 等待 attempt deadline，模拟数据库锁或网络阻塞。
func (repository *deadlineRepository) ResolveMailboxOperation(ctx context.Context, _ Resolution) error {
	close(repository.resolveEntered)
	<-ctx.Done()
	return ctx.Err()
}

// ClaimNext 只在第一次调用返回任务，后续模拟空队列。
func (executor *executorFake) ClaimNext(
	_ context.Context,
) (ClaimedMailboxOperation, bool, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.claimCalls++
	if executor.claimCalls == 1 && executor.hasClaim {
		return executor.claim, true, nil
	}
	return ClaimedMailboxOperation{}, false, nil
}

// Execute 等待测试释放，用于证明父取消不会中断已经领取的 attempt。
func (executor *executorFake) Execute(ctx context.Context, _ ClaimedMailboxOperation) error {
	executor.mu.Lock()
	executor.executeCalls++
	executor.mu.Unlock()
	close(executor.entered)
	select {
	case <-executor.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// counts 返回 runner 调用次数的并发安全快照。
func (executor *executorFake) counts() (int, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.claimCalls, executor.executeCalls
}

func TestRunnerStopsClaimingButFinishesCurrentAttemptAfterCancellation(t *testing.T) {
	executor := &executorFake{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		hasClaim: true,
	}
	runner, err := NewRunner(executor, time.Millisecond)
	if err != nil {
		t.Fatalf("创建 runner 失败：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	select {
	case <-executor.entered:
	case <-time.After(time.Second):
		t.Fatal("runner 未执行已领取任务")
	}
	cancel()
	select {
	case err = <-done:
		t.Fatalf("runner 在当前 attempt 收尾前提前退出：%v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(executor.release)
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("runner 关闭失败：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner 未在当前 attempt 完成后退出")
	}
	claimCalls, executeCalls := executor.counts()
	if claimCalls != 1 || executeCalls != 1 {
		t.Fatalf("取消后不得领取新任务：claim=%d execute=%d", claimCalls, executeCalls)
	}
}

func TestRunnerCancelsEmptyQueueWait(t *testing.T) {
	executor := &executorFake{}
	runner, err := NewRunner(executor, time.Hour)
	if err != nil {
		t.Fatalf("创建 runner 失败：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	deadline := time.Now().Add(time.Second)
	for {
		claimCalls, _ := executor.counts()
		if claimCalls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner 未扫描空队列")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("空队列取消返回错误：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("空队列等待未响应取消")
	}
}

func TestRunnerFinishesCanceledAttemptWhenRepositoryBlocksUntilAttemptDeadline(t *testing.T) {
	_, _, adapter, claimed := newOperationTestFixture(t)
	now := time.Now().UTC()
	claimed.LeaseExpiresAt = now.Add(200 * time.Millisecond)
	repository := &deadlineRepository{
		claim:          claimed,
		resolveEntered: make(chan struct{}),
	}
	service, err := NewService(repository, adapter, Config{
		OwnerID:           claimed.LeaseOwnerID,
		LeaseDuration:     200 * time.Millisecond,
		AttemptTimeout:    30 * time.Millisecond,
		LeaseSafetyMargin: 20 * time.Millisecond,
		BaseRetryDelay:    time.Millisecond,
		MaxRetryDelay:     time.Second,
		MaxAttempts:       3,
	})
	if err != nil {
		t.Fatalf("创建带回执 deadline 的 service 失败：%v", err)
	}
	runner, err := NewRunner(service, time.Millisecond)
	if err != nil {
		t.Fatalf("创建 runner 失败：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	select {
	case <-repository.resolveEntered:
	case <-time.After(time.Second):
		t.Fatal("runner 未进入数据库回执")
	}
	cancel()

	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("关闭期间 attempt deadline 到期应正常结束 runner：%v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("repository 阻塞时 runner 未在 attempt deadline 内结束")
	}
}
