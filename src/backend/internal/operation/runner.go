package operation

import (
	"context"
	"errors"
	"time"
)

// Executor 定义 runner 领取和执行单个 operation 的最小边界。
type Executor interface {
	// ClaimNext 从持久化 ledger 领取一个到期 operation。
	ClaimNext(context.Context) (ClaimedMailboxOperation, bool, error)
	// Execute 在已领取 lease 的有界期限内执行并提交结果。
	Execute(context.Context, ClaimedMailboxOperation) error
}

// Runner 持续扫描 PostgreSQL ledger，并在关闭时停止领取新任务。
type Runner struct {
	executor     Executor
	pollInterval time.Duration
}

// NewRunner 创建不依赖进程内通知正确性的持久化任务扫描器。
func NewRunner(executor Executor, pollInterval time.Duration) (*Runner, error) {
	if executor == nil || pollInterval <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Runner{executor: executor, pollInterval: pollInterval}, nil
}

// Run 处理到期 operation；父上下文取消后不再领取，但已领取 attempt 可在自身期限内结束。
func (runner *Runner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		claimed, found, err := runner.executor.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !found {
			if !waitForNextPoll(ctx, runner.pollInterval) {
				return nil
			}
			continue
		}

		// Execute 自行按 attempt/lease 设置 deadline；这里只移除进程取消，允许已领取任务有界收尾。
		if err = runner.executor.Execute(context.WithoutCancel(ctx), claimed); err != nil &&
			!errors.Is(err, ErrLeaseLost) {
			if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

// waitForNextPoll 等待下一轮扫描，并允许关闭信号立即中断空队列等待。
func waitForNextPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
