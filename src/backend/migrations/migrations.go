// Package migrations 提供嵌入制品的控制面数据库迁移入口。
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/database"
)

// migrationFiles 将已审阅的 SQL migration 固定进 migrator 制品，避免运行时目录漂移。
//
//go:embed *.sql
var migrationFiles embed.FS

// Command 表示 migrator 唯一允许执行的数据库动作。
type Command string

const (
	// CommandUp 应用全部待执行 migration。
	CommandUp Command = "up"
	// CommandDown 回滚最后一个已应用 migration。
	CommandDown Command = "down"
	// CommandStatus 只读取并输出 migration 状态。
	CommandStatus Command = "status"
)

// Run 连接控制面数据库并执行单个显式 migration 命令。
func Run(ctx context.Context, databaseURL string, command Command, output io.Writer) (runErr error) {
	if err := database.Validate(databaseURL); err != nil {
		return err
	}

	connectionConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return errors.New("数据库连接配置无效")
	}
	db := stdlib.OpenDB(*connectionConfig)
	defer func() {
		if closeErr := db.Close(); runErr == nil && closeErr != nil {
			runErr = errors.New("数据库迁移连接关闭失败")
		}
	}()

	sessionLocker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return errors.New("数据库迁移锁初始化失败")
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrationFiles,
		goose.WithDisableGlobalRegistry(true),
		goose.WithSessionLocker(sessionLocker),
	)
	if err != nil {
		return errors.New("数据库迁移器初始化失败")
	}

	switch command {
	case CommandUp:
		return runUp(ctx, provider, output)
	case CommandDown:
		return runDown(ctx, provider, output)
	case CommandStatus:
		return writeStatus(ctx, provider, output)
	default:
		return errors.New("数据库迁移命令无效")
	}
}

// runUp 应用全部待执行版本，并只输出稳定版本号而不输出数据库拓扑。
func runUp(ctx context.Context, provider *goose.Provider, output io.Writer) error {
	results, err := provider.Up(ctx)
	if err != nil {
		return errors.New("数据库 migration 应用失败")
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(output, "数据库 schema 已是最新版本")
		return nil
	}
	_, _ = fmt.Fprintf(output, "已应用 %d 个 migration，当前版本 %d\n", len(results), results[len(results)-1].Source.Version)
	return nil
}

// runDown 回滚最后一个已应用版本；该动作只能由显式 --down 触发。
func runDown(ctx context.Context, provider *goose.Provider, output io.Writer) error {
	result, err := provider.Down(ctx)
	if err != nil {
		return errors.New("数据库 migration 回滚失败")
	}
	_, _ = fmt.Fprintf(output, "已回滚 migration %d\n", result.Source.Version)
	return nil
}

// writeStatus 输出 migration 版本及 applied/pending 状态，不包含连接或迁移时间信息。
func writeStatus(ctx context.Context, provider *goose.Provider, output io.Writer) error {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return errors.New("数据库 migration 状态读取失败")
	}
	for _, status := range statuses {
		_, _ = fmt.Fprintf(output, "%05d %s\n", status.Source.Version, status.State)
	}
	return nil
}
