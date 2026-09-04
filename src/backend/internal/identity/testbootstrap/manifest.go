// Package testbootstrap 提供受控 OIDC 集成测试身份的显式初始化与回收能力。
package testbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	manifestSchemaVersion = 1
	maxManifestBytes      = 64 * 1024
)

var (
	// localPartPattern 与数据库当前允许的 ASCII 邮箱本地部分字符集保持一致。
	localPartPattern = regexp.MustCompile(`^[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+$`)
	// domainLabelPattern 限制测试域名使用规范化后的 ASCII DNS label。
	domainLabelPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// Manifest 描述一个无秘密、可重复核验的 OIDC 集成测试身份矩阵。
type Manifest struct {
	// SchemaVersion 固定 manifest 结构版本，防止旧命令误读新字段。
	SchemaVersion int `json:"schema_version"`
	// OIDCIssuer 是所有测试主体共享且按 OIDC exact-match 保存的 HTTPS 签发方。
	OIDCIssuer string `json:"oidc_issuer"`
	// Tenant 是 fixture 独占的控制面测试租户。
	Tenant TenantManifest `json:"tenant"`
	// Domain 是 fixture 独占且位于 .test 保留顶级域的邮箱域名。
	Domain DomainManifest `json:"domain"`
	// Mailbox 是能够正常登录的 active 邮箱主体。
	Mailbox MailboxPrincipalManifest `json:"mailbox"`
	// Administrator 是具备管理入口权限的 active 管理主体。
	Administrator AdministratorPrincipalManifest `json:"administrator"`
	// Suspended 是必须被本地主体状态拒绝的 suspended 邮箱主体。
	Suspended MailboxPrincipalManifest `json:"suspended"`
	// MFAInsufficient 是映射和权限正常、但应被 IdP MFA 证据拒绝的管理主体。
	MFAInsufficient AdministratorPrincipalManifest `json:"mfa_insufficient"`
	// UnmappedSubject 是必须刻意不存在于本地主体表的 OIDC subject。
	UnmappedSubject string `json:"unmapped_subject"`
}

// TenantManifest 固定测试租户的稳定标识和显示名称。
type TenantManifest struct {
	// ID 是测试租户跨重复执行保持不变的 UUID。
	ID uuid.UUID `json:"id"`
	// Name 是测试租户的非敏感显示名称。
	Name string `json:"name"`
}

// DomainManifest 固定测试邮件域的稳定标识和保留域名。
type DomainManifest struct {
	// ID 是测试域名跨重复执行保持不变的 UUID。
	ID uuid.UUID `json:"id"`
	// Name 是小写规范化且以 .test 结尾的测试域名。
	Name string `json:"name"`
}

// MailboxPrincipalManifest 把一个 OIDC subject 映射到独立的同租户测试邮箱。
type MailboxPrincipalManifest struct {
	// PrincipalID 是本地认证主体的稳定 UUID。
	PrincipalID uuid.UUID `json:"principal_id"`
	// Subject 是 IdP 提供的不透明且稳定的 OIDC subject。
	Subject string `json:"subject"`
	// MailboxID 是绑定邮箱的稳定 UUID。
	MailboxID uuid.UUID `json:"mailbox_id"`
	// LocalPart 是测试邮箱地址的小写 ASCII 本地部分。
	LocalPart string `json:"local_part"`
	// DisplayName 同时作为邮箱和主体的测试显示名称。
	DisplayName string `json:"display_name"`
}

// AdministratorPrincipalManifest 描述不绑定邮箱的本地管理主体。
type AdministratorPrincipalManifest struct {
	// PrincipalID 是本地认证主体的稳定 UUID。
	PrincipalID uuid.UUID `json:"principal_id"`
	// Subject 是 IdP 提供的不透明且稳定的 OIDC subject。
	Subject string `json:"subject"`
	// DisplayName 是管理主体的测试显示名称。
	DisplayName string `json:"display_name"`
}

// LoadManifest 严格解析大小受限的 JSON，并在返回前完成全部无副作用校验。
func LoadManifest(reader io.Reader) (Manifest, error) {
	if reader == nil {
		return Manifest{}, errors.New("测试身份 manifest 不能为空")
	}
	content, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, errors.New("读取测试身份 manifest 失败")
	}
	if len(content) == 0 || len(content) > maxManifestBytes {
		return Manifest{}, errors.New("测试身份 manifest 大小无效")
	}

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err = decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("测试身份 manifest JSON 结构无效")
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("测试身份 manifest 只能包含一个 JSON 对象")
	}
	if err = manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate 核验 manifest 的测试数据边界，不连接数据库或修改调用方输入。
func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != manifestSchemaVersion {
		return errors.New("测试身份 manifest schema_version 必须为 1")
	}
	if err := validateIssuer(manifest.OIDCIssuer); err != nil {
		return err
	}
	if manifest.Tenant.ID == uuid.Nil || !validDisplayName(manifest.Tenant.Name) {
		return errors.New("测试身份 manifest 的租户无效")
	}
	if manifest.Domain.ID == uuid.Nil || !validTestDomain(manifest.Domain.Name) {
		return errors.New("测试身份 manifest 的域名必须是小写 .test 保留域")
	}
	if err := validateMailboxPrincipal("mailbox", manifest.Mailbox); err != nil {
		return err
	}
	if err := validateAdministratorPrincipal("administrator", manifest.Administrator); err != nil {
		return err
	}
	if err := validateMailboxPrincipal("suspended", manifest.Suspended); err != nil {
		return err
	}
	if err := validateAdministratorPrincipal("mfa_insufficient", manifest.MFAInsufficient); err != nil {
		return err
	}
	if !validSubject(manifest.UnmappedSubject) {
		return errors.New("测试身份 manifest 的 unmapped_subject 无效")
	}
	if manifest.Mailbox.LocalPart == manifest.Suspended.LocalPart {
		return errors.New("测试身份 manifest 的邮箱本地部分必须互不相同")
	}

	identifiers := []uuid.UUID{
		manifest.Tenant.ID,
		manifest.Domain.ID,
		manifest.Mailbox.MailboxID,
		manifest.Mailbox.PrincipalID,
		manifest.Administrator.PrincipalID,
		manifest.Suspended.MailboxID,
		manifest.Suspended.PrincipalID,
		manifest.MFAInsufficient.PrincipalID,
	}
	seenIdentifiers := make(map[uuid.UUID]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if _, exists := seenIdentifiers[identifier]; exists {
			return errors.New("测试身份 manifest 的 UUID 必须互不相同")
		}
		seenIdentifiers[identifier] = struct{}{}
	}

	subjects := []string{
		manifest.Mailbox.Subject,
		manifest.Administrator.Subject,
		manifest.Suspended.Subject,
		manifest.MFAInsufficient.Subject,
		manifest.UnmappedSubject,
	}
	seenSubjects := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		if _, exists := seenSubjects[subject]; exists {
			return errors.New("测试身份 manifest 的 OIDC subject 必须互不相同")
		}
		seenSubjects[subject] = struct{}{}
	}
	return nil
}

// validateIssuer 要求测试 issuer 满足生产 OIDC 所需的精确 HTTPS URL 边界。
func validateIssuer(issuer string) error {
	if issuer != strings.TrimSpace(issuer) || len(issuer) == 0 || len(issuer) > 2048 || hasControl(issuer) {
		return errors.New("测试身份 manifest 的 oidc_issuer 无效")
	}
	parsed, err := url.ParseRequestURI(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("测试身份 manifest 的 oidc_issuer 必须是 HTTPS URL")
	}
	return nil
}

// validateMailboxPrincipal 核验邮箱主体及其绑定邮箱的稳定字段。
func validateMailboxPrincipal(name string, principal MailboxPrincipalManifest) error {
	if principal.PrincipalID == uuid.Nil || principal.MailboxID == uuid.Nil ||
		!validSubject(principal.Subject) || !validLocalPart(principal.LocalPart) ||
		!validDisplayName(principal.DisplayName) {
		return errors.New("测试身份 manifest 的 " + name + " 主体无效")
	}
	return nil
}

// validateAdministratorPrincipal 核验管理主体的稳定字段。
func validateAdministratorPrincipal(name string, principal AdministratorPrincipalManifest) error {
	if principal.PrincipalID == uuid.Nil || !validSubject(principal.Subject) ||
		!validDisplayName(principal.DisplayName) {
		return errors.New("测试身份 manifest 的 " + name + " 主体无效")
	}
	return nil
}

// validTestDomain 仅接受不会进入公共 DNS 的保留测试顶级域。
func validTestDomain(domain string) bool {
	if domain != strings.ToLower(domain) || domain != strings.TrimSpace(domain) ||
		len(domain) < len("a.test") || len(domain) > 253 || !strings.HasSuffix(domain, ".test") ||
		strings.HasSuffix(domain, ".") || hasControl(domain) {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) < 1 || len(label) > 63 || !domainLabelPattern.MatchString(label) ||
			strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

// validLocalPart 与数据库邮箱本地部分约束保持一致。
func validLocalPart(localPart string) bool {
	return localPart == strings.ToLower(localPart) && len(localPart) >= 1 && len(localPart) <= 64 &&
		localPartPattern.MatchString(localPart) && !strings.HasPrefix(localPart, ".") &&
		!strings.HasSuffix(localPart, ".") && !strings.Contains(localPart, "..")
}

// validSubject 与本地主体数据库约束保持一致，并保留 OIDC subject 的大小写语义。
func validSubject(subject string) bool {
	return len(subject) >= 1 && len(subject) <= 255 && !hasControl(subject)
}

// validDisplayName 保证界面测试名称已规范化且符合数据库字符长度约束。
func validDisplayName(name string) bool {
	return name == strings.TrimSpace(name) && utf8.RuneCountInString(name) >= 1 &&
		utf8.RuneCountInString(name) <= 128 && !hasControl(name)
}

// hasControl 拒绝会破坏日志、配置或数据库文本边界的控制字符。
func hasControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
