// Package main 提供独立数据库迁移进程入口。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/config"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/database"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 解析无副作用的脚手架命令；业务 migration 需后续契约引入。
func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "输出 migrator 构建版本")
	checkConfig := flags.Bool("check-config", false, "校验数据库配置但不连接或执行 DDL")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "用法：migrator --version | --check-config")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || (*showVersion && *checkConfig) {
		flags.Usage()
		return 2
	}

	switch {
	case *showVersion:
		_, _ = fmt.Fprintln(stdout, buildinfo.Version)
		return 0
	case *checkConfig:
		serviceConfig, err := config.Load("migrator", "127.0.0.1:0")
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if err := database.Validate(serviceConfig.DatabaseURL); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "配置有效；当前脚手架不包含业务 migration")
		return 0
	default:
		flags.Usage()
		return 2
	}
}
