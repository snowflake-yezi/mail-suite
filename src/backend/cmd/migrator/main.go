// Package main 提供独立数据库迁移进程入口。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/config"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/database"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, migrations.Run))
}

// migrationRunner 注入可测试的显式 migration 执行边界。
type migrationRunner func(context.Context, string, migrations.Command, io.Writer) error

// run 解析唯一 migrator 动作，并为配置或执行错误返回稳定退出码。
func run(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	execute migrationRunner,
) int {
	flags := flag.NewFlagSet("migrator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "输出 migrator 构建版本")
	checkConfig := flags.Bool("check-config", false, "校验数据库配置但不连接或执行 DDL")
	applyUp := flags.Bool("up", false, "应用全部待执行数据库 migration")
	applyDown := flags.Bool("down", false, "回滚最后一个已应用数据库 migration")
	showStatus := flags.Bool("status", false, "查看数据库 migration 状态")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "用法：migrator --version | --check-config | --up | --down | --status")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	selectedCommands := countSelected(*showVersion, *checkConfig, *applyUp, *applyDown, *showStatus)
	if flags.NArg() != 0 || selectedCommands != 1 {
		flags.Usage()
		return 2
	}

	switch {
	case *showVersion:
		_, _ = fmt.Fprintln(stdout, buildinfo.Version)
		return 0
	case *checkConfig:
		serviceConfig, ok := loadDatabaseConfig(stderr)
		if !ok {
			return 1
		}
		if err := database.Validate(serviceConfig.DatabaseURL); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "配置有效；未连接数据库或执行 migration")
		return 0
	default:
		serviceConfig, ok := loadDatabaseConfig(stderr)
		if !ok {
			return 1
		}
		command := migrations.CommandStatus
		switch {
		case *applyUp:
			command = migrations.CommandUp
		case *applyDown:
			command = migrations.CommandDown
		}
		if err := execute(ctx, serviceConfig.DatabaseURL, command, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
}

// countSelected 统计互斥布尔命令，防止一次进程执行多个数据库动作。
func countSelected(commands ...bool) int {
	selected := 0
	for _, command := range commands {
		if command {
			selected++
		}
	}
	return selected
}

// loadDatabaseConfig 读取 migrator 配置，并将脱敏错误写入调用方提供的输出。
func loadDatabaseConfig(stderr io.Writer) (config.Config, bool) {
	serviceConfig, err := config.Load("migrator", "127.0.0.1:0")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return config.Config{}, false
	}
	return serviceConfig, true
}
