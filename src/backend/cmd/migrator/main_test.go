package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsVersionWithoutDatabaseConfiguration(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--version"}, &stdout, &stderr)
	if exitCode != 0 || strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("版本命令失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunCheckConfigHasNoDatabaseSideEffect(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"--check-config"}, &stdout, &stderr)
	if exitCode != 0 || !strings.Contains(stdout.String(), "不包含业务 migration") {
		t.Fatalf("配置检查失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunRejectsMissingCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run(nil, &stdout, &stderr)
	if exitCode != 2 || !strings.Contains(stderr.String(), "用法") {
		t.Fatalf("无命令时应返回用法错误：exit=%d stderr=%q", exitCode, stderr.String())
	}
}
