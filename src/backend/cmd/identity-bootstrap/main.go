// Package main 提供受控测试身份 fixture 的独立初始化和回收入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/testbootstrap"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/database"
)

// action 标识一次命令允许执行的唯一 fixture 操作。
type action string

const (
	actionApply  action = "apply"
	actionRemove action = "remove"
)

// main 建立可取消的命令上下文并把稳定退出码交还操作系统。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, loadManifestFile, execute))
}

// manifestLoader 注入可测试的 manifest 文件读取边界。
type manifestLoader func(string) (testbootstrap.Manifest, error)

// bootstrapRunner 注入可测试的数据库连接和 fixture 事务边界。
type bootstrapRunner func(
	context.Context,
	string,
	action,
	testbootstrap.Manifest,
) (testbootstrap.Result, error)

// run 解析唯一动作，校验无秘密配置，并为参数或执行失败返回稳定退出码。
func run(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	loadManifest manifestLoader,
	executeBootstrap bootstrapRunner,
) int {
	flags := flag.NewFlagSet("identity-bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "输出 identity-bootstrap 构建版本")
	checkConfig := flags.Bool("check", false, "校验数据库配置和 manifest，但不连接数据库")
	applyFixture := flags.Bool("apply", false, "创建或核验测试身份 fixture")
	removeFixture := flags.Bool("remove", false, "显式回收测试身份 fixture")
	manifestPath := flags.String("manifest", "", "无秘密测试身份 JSON manifest 路径")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(
			stderr,
			"用法：identity-bootstrap --version | (--check | --apply | --remove) --manifest <path>",
		)
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || countSelected(*showVersion, *checkConfig, *applyFixture, *removeFixture) != 1 {
		flags.Usage()
		return 2
	}
	if *showVersion {
		if strings.TrimSpace(*manifestPath) != "" {
			flags.Usage()
			return 2
		}
		_, _ = fmt.Fprintln(stdout, buildinfo.Version)
		return 0
	}
	if strings.TrimSpace(*manifestPath) == "" {
		flags.Usage()
		return 2
	}

	manifest, err := loadManifest(*manifestPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	databaseURL, err := loadDatabaseURL()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err = database.Validate(databaseURL); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if *checkConfig {
		_, _ = fmt.Fprintln(stdout, "配置与测试身份 manifest 有效；未连接数据库")
		return 0
	}

	selectedAction := actionApply
	if *removeFixture {
		selectedAction = actionRemove
	}
	result, err := executeBootstrap(ctx, databaseURL, selectedAction, manifest)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	writeResult(stdout, selectedAction, result)
	return 0
}

// countSelected 统计互斥命令选项，防止一次调用混合只读和写入动作。
func countSelected(commands ...bool) int {
	selected := 0
	for _, command := range commands {
		if command {
			selected++
		}
	}
	return selected
}

// loadManifestFile 从显式路径读取 manifest，不解析目录或使用隐式默认文件。
func loadManifestFile(path string) (testbootstrap.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return testbootstrap.Manifest{}, errors.New("无法打开测试身份 manifest")
	}
	defer func() { _ = file.Close() }()
	return testbootstrap.LoadManifest(file)
}

// loadDatabaseURL 读取控制面连接串，但从不把原值写入错误或输出。
func loadDatabaseURL() (string, error) {
	databaseURL := strings.TrimSpace(os.Getenv("MAIL_SUITE_DATABASE_URL"))
	if databaseURL == "" {
		return "", errors.New("缺少必需配置 MAIL_SUITE_DATABASE_URL")
	}
	return databaseURL, nil
}

// execute 建立一次性连接池并执行明确的 apply 或 remove 事务。
func execute(
	ctx context.Context,
	databaseURL string,
	selectedAction action,
	manifest testbootstrap.Manifest,
) (testbootstrap.Result, error) {
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		return testbootstrap.Result{}, err
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return testbootstrap.Result{}, errors.New("控制面数据库不可用")
	}
	return executeWithPool(ctx, pool, selectedAction, manifest)
}

// executeWithPool 把命令动作映射到唯一数据库事务服务。
func executeWithPool(
	ctx context.Context,
	pool *pgxpool.Pool,
	selectedAction action,
	manifest testbootstrap.Manifest,
) (testbootstrap.Result, error) {
	switch selectedAction {
	case actionApply:
		return testbootstrap.Apply(ctx, pool, manifest)
	case actionRemove:
		return testbootstrap.Remove(ctx, pool, manifest)
	default:
		return testbootstrap.Result{}, errors.New("测试身份操作无效")
	}
}

// writeResult 输出不包含 issuer、subject、UUID、邮箱或连接信息的稳定结果。
func writeResult(output io.Writer, selectedAction action, result testbootstrap.Result) {
	switch {
	case selectedAction == actionApply && result.Changed:
		_, _ = fmt.Fprintln(output, "测试身份 fixture 已创建")
	case selectedAction == actionApply:
		_, _ = fmt.Fprintln(output, "测试身份 fixture 已匹配；无变更")
	case result.Changed:
		_, _ = fmt.Fprintln(output, "测试身份 fixture 已回收")
	default:
		_, _ = fmt.Fprintln(output, "测试身份 fixture 已不存在；无变更")
	}
}
