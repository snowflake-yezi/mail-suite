package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/testbootstrap"
)

func TestRunVersionDoesNotRequireDatabaseOrManifest(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"--version"}, &stdout, &stderr, nil, nil)
	if exitCode != 0 || strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("版本命令失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunCheckValidatesWithoutDatabaseConnection(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	called := false
	loader := func(path string) (testbootstrap.Manifest, error) {
		if path != "fixture.json" {
			t.Fatalf("manifest 路径错误：%q", path)
		}
		return commandTestManifest(), nil
	}
	runner := func(
		context.Context,
		string,
		action,
		testbootstrap.Manifest,
	) (testbootstrap.Result, error) {
		called = true
		return testbootstrap.Result{}, nil
	}
	exitCode := run(
		context.Background(),
		[]string{"--check", "--manifest", "fixture.json"},
		&stdout,
		&stderr,
		loader,
		runner,
	)
	if exitCode != 0 || called || !strings.Contains(stdout.String(), "未连接数据库") {
		t.Fatalf("只读检查边界错误：exit=%d called=%t stdout=%q stderr=%q", exitCode, called, stdout.String(), stderr.String())
	}
}

func TestRunDispatchesApplyAndWritesNonsensitiveResult(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	manifest := commandTestManifest()
	loader := func(string) (testbootstrap.Manifest, error) { return manifest, nil }
	runner := func(
		_ context.Context,
		_ string,
		selectedAction action,
		got testbootstrap.Manifest,
	) (testbootstrap.Result, error) {
		if selectedAction != actionApply || got.OIDCIssuer != manifest.OIDCIssuer {
			t.Fatalf("apply 分派错误：action=%q manifest=%+v", selectedAction, got)
		}
		return testbootstrap.Result{Changed: true}, nil
	}
	exitCode := run(
		context.Background(),
		[]string{"--apply", "--manifest", "fixture.json"},
		&stdout,
		&stderr,
		loader,
		runner,
	)
	if exitCode != 0 || !strings.Contains(stdout.String(), "已创建") {
		t.Fatalf("apply 命令失败：exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), manifest.OIDCIssuer) || strings.Contains(stdout.String(), manifest.Mailbox.Subject) {
		t.Fatal("成功输出不得包含 issuer 或 subject")
	}
}

func TestRunRejectsAmbiguousActionsAndMissingInputs(t *testing.T) {
	tests := [][]string{
		nil,
		{"--apply"},
		{"--check", "--apply", "--manifest", "fixture.json"},
		{"--version", "--manifest", "fixture.json"},
	}
	for _, arguments := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		exitCode := run(context.Background(), arguments, &stdout, &stderr, nil, nil)
		if exitCode != 2 || !strings.Contains(stderr.String(), "用法") {
			t.Fatalf("歧义参数必须返回用法错误：args=%v exit=%d stderr=%q", arguments, exitCode, stderr.String())
		}
	}
}

func TestRunHidesManifestPathAndStopsBeforeExecutionOnLoadFailure(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "postgres://mail_suite@127.0.0.1/mail_suite?sslmode=disable")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	secretLikePath := "fixture-with-private-value.json"
	loader := func(string) (testbootstrap.Manifest, error) {
		return testbootstrap.Manifest{}, errors.New("无法打开测试身份 manifest")
	}
	exitCode := run(
		context.Background(),
		[]string{"--apply", "--manifest", secretLikePath},
		&stdout,
		&stderr,
		loader,
		nil,
	)
	if exitCode != 1 || strings.Contains(stderr.String(), secretLikePath) {
		t.Fatalf("manifest 失败输出不应回显路径：exit=%d stderr=%q", exitCode, stderr.String())
	}
}

func TestRunRequiresDatabaseConfigurationBeforeExecution(t *testing.T) {
	t.Setenv("MAIL_SUITE_DATABASE_URL", "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	loader := func(string) (testbootstrap.Manifest, error) { return commandTestManifest(), nil }
	exitCode := run(
		context.Background(),
		[]string{"--remove", "--manifest", "fixture.json"},
		&stdout,
		&stderr,
		loader,
		nil,
	)
	if exitCode != 1 || !strings.Contains(stderr.String(), "MAIL_SUITE_DATABASE_URL") {
		t.Fatalf("缺少数据库配置应快速失败：exit=%d stderr=%q", exitCode, stderr.String())
	}
}

// commandTestManifest 返回仅供 CLI 分派测试使用的合法无秘密 manifest。
func commandTestManifest() testbootstrap.Manifest {
	return testbootstrap.Manifest{
		SchemaVersion: 1,
		OIDCIssuer:    "https://idp.example.test/realms/mail-suite",
		Tenant: testbootstrap.TenantManifest{
			ID:   uuid.MustParse("20000000-0000-4000-8000-000000000001"),
			Name: "CLI 测试租户",
		},
		Domain: testbootstrap.DomainManifest{
			ID:   uuid.MustParse("20000000-0000-4000-8000-000000000002"),
			Name: "cli.example.test",
		},
		Mailbox: testbootstrap.MailboxPrincipalManifest{
			PrincipalID: uuid.MustParse("20000000-0000-4000-8000-000000000003"),
			Subject:     "mailbox-subject",
			MailboxID:   uuid.MustParse("20000000-0000-4000-8000-000000000004"),
			LocalPart:   "mailbox",
			DisplayName: "Mailbox Fixture",
		},
		Administrator: testbootstrap.AdministratorPrincipalManifest{
			PrincipalID: uuid.MustParse("20000000-0000-4000-8000-000000000005"),
			Subject:     "administrator-subject",
			DisplayName: "Administrator Fixture",
		},
		Suspended: testbootstrap.MailboxPrincipalManifest{
			PrincipalID: uuid.MustParse("20000000-0000-4000-8000-000000000006"),
			Subject:     "suspended-subject",
			MailboxID:   uuid.MustParse("20000000-0000-4000-8000-000000000007"),
			LocalPart:   "suspended",
			DisplayName: "Suspended Fixture",
		},
		MFAInsufficient: testbootstrap.AdministratorPrincipalManifest{
			PrincipalID: uuid.MustParse("20000000-0000-4000-8000-000000000008"),
			Subject:     "mfa-insufficient-subject",
			DisplayName: "MFA Insufficient Fixture",
		},
		UnmappedSubject: "unmapped-subject",
	}
}
