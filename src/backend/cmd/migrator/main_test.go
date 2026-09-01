package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

func TestRunPrintsVersionWithoutDatabaseConfiguration(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), []string{"--version"}, &stdout, &stderr, nil)
	if exitCode != 0 || strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("版本命令失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunCheckConfigHasNoDatabaseSideEffect(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), []string{"--check-config"}, &stdout, &stderr, nil)
	if exitCode != 0 || !strings.Contains(stdout.String(), "未连接数据库") {
		t.Fatalf("配置检查失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunRejectsMissingCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), nil, &stdout, &stderr, nil)
	if exitCode != 2 || !strings.Contains(stderr.String(), "用法") {
		t.Fatalf("无命令时应返回用法错误：exit=%d stderr=%q", exitCode, stderr.String())
	}
}

func TestRunDispatchesExactlyOneMigrationCommand(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var got migrations.Command
	runner := func(
		_ context.Context,
		_ string,
		command migrations.Command,
		_ io.Writer,
	) error {
		got = command
		return nil
	}

	exitCode := run(context.Background(), []string{"--up"}, &stdout, &stderr, runner)
	if exitCode != 0 || got != migrations.CommandUp {
		t.Fatalf("up 命令分派失败：exit=%d command=%q stderr=%q", exitCode, got, stderr.String())
	}
}

func TestRunRejectsMultipleCommands(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), []string{"--up", "--status"}, &stdout, &stderr, nil)
	if exitCode != 2 || !strings.Contains(stderr.String(), "用法") {
		t.Fatalf("多个命令应返回用法错误：exit=%d stderr=%q", exitCode, stderr.String())
	}
}

func TestRunMigrationCommandRequiresDatabaseConfiguration(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), []string{"--up"}, &stdout, &stderr, nil)
	if exitCode != 1 || !strings.Contains(stderr.String(), "MAIL_SUITE_DATABASE_URL") {
		t.Fatalf("缺少数据库配置应快速失败：exit=%d stderr=%q", exitCode, stderr.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(context.Background(), []string{"--unknown"}, &stdout, &stderr, nil)
	if exitCode != 2 || !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("未知命令应返回参数错误：exit=%d stderr=%q", exitCode, stderr.String())
	}
}
