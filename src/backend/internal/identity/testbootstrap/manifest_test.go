package testbootstrap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLoadManifestAcceptsCompleteTestIdentityMatrix(t *testing.T) {
	content, err := json.Marshal(validTestManifest())
	if err != nil {
		t.Fatalf("编码合法测试 manifest 失败：%v", err)
	}
	manifest, err := LoadManifest(strings.NewReader(string(content)))
	if err != nil {
		t.Fatalf("合法测试身份 manifest 应通过：%v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.UnmappedSubject == "" {
		t.Fatalf("解析后的测试身份 manifest 不完整：%+v", manifest)
	}
}

func TestLoadManifestRejectsUnknownFieldAndTrailingDocument(t *testing.T) {
	content, err := json.Marshal(validTestManifest())
	if err != nil {
		t.Fatalf("编码合法测试 manifest 失败：%v", err)
	}
	withUnknownField := strings.Replace(
		string(content),
		`{"schema_version":`,
		`{"unexpected":true,"schema_version":`,
		1,
	)
	if _, err = LoadManifest(strings.NewReader(withUnknownField)); err == nil {
		t.Fatal("未知 manifest 字段必须被拒绝")
	}
	if _, err = LoadManifest(strings.NewReader(string(content) + `{}`)); err == nil {
		t.Fatal("尾随 JSON 文档必须被拒绝")
	}
}

func TestManifestValidateRejectsUnsafeOrAmbiguousFixtureValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{
			name: "非 HTTPS issuer",
			mutate: func(manifest *Manifest) {
				manifest.OIDCIssuer = "http://idp.example.test/realms/mail-suite"
			},
		},
		{
			name: "真实可投递域名",
			mutate: func(manifest *Manifest) {
				manifest.Domain.Name = "mail.example.com"
			},
		},
		{
			name: "重复 subject",
			mutate: func(manifest *Manifest) {
				manifest.UnmappedSubject = manifest.Mailbox.Subject
			},
		},
		{
			name: "重复 UUID",
			mutate: func(manifest *Manifest) {
				manifest.MFAInsufficient.PrincipalID = manifest.Administrator.PrincipalID
			},
		},
		{
			name: "非法邮箱本地部分",
			mutate: func(manifest *Manifest) {
				manifest.Mailbox.LocalPart = "Alice"
			},
		},
		{
			name: "显示名未规范化",
			mutate: func(manifest *Manifest) {
				manifest.Suspended.DisplayName = " Suspended "
			},
		},
		{
			name: "空 UUID",
			mutate: func(manifest *Manifest) {
				manifest.Domain.ID = uuid.Nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validTestManifest()
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("不安全或歧义 fixture 必须被拒绝")
			}
		})
	}
}

// validTestManifest 返回只使用保留测试域和固定假身份的完整 manifest。
func validTestManifest() Manifest {
	return Manifest{
		SchemaVersion: 1,
		OIDCIssuer:    "https://idp.example.test/realms/mail-suite",
		Tenant: TenantManifest{
			ID:   uuid.MustParse("10000000-0000-4000-8000-000000000001"),
			Name: "OIDC 集成测试租户",
		},
		Domain: DomainManifest{
			ID:   uuid.MustParse("10000000-0000-4000-8000-000000000002"),
			Name: "mail-suite.example.test",
		},
		Mailbox: MailboxPrincipalManifest{
			PrincipalID: uuid.MustParse("10000000-0000-4000-8000-000000000003"),
			Subject:     "mailbox-subject",
			MailboxID:   uuid.MustParse("10000000-0000-4000-8000-000000000004"),
			LocalPart:   "mailbox",
			DisplayName: "Mailbox Fixture",
		},
		Administrator: AdministratorPrincipalManifest{
			PrincipalID: uuid.MustParse("10000000-0000-4000-8000-000000000005"),
			Subject:     "administrator-subject",
			DisplayName: "Administrator Fixture",
		},
		Suspended: MailboxPrincipalManifest{
			PrincipalID: uuid.MustParse("10000000-0000-4000-8000-000000000006"),
			Subject:     "suspended-subject",
			MailboxID:   uuid.MustParse("10000000-0000-4000-8000-000000000007"),
			LocalPart:   "suspended",
			DisplayName: "Suspended Fixture",
		},
		MFAInsufficient: AdministratorPrincipalManifest{
			PrincipalID: uuid.MustParse("10000000-0000-4000-8000-000000000008"),
			Subject:     "mfa-insufficient-subject",
			DisplayName: "MFA Insufficient Fixture",
		},
		UnmappedSubject: "unmapped-subject",
	}
}
