package stalwart

import (
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadConfigReadsRestrictedSecretFiles 验证环境只保存秘密路径且 Base64 派生密钥被严格解码。
func TestLoadConfigReadsRestrictedSecretFiles(t *testing.T) {
	directory := t.TempDir()
	passwordPath := writeTestSecret(t, directory, "admin-password", "test-password")
	mailboxKey := make([]byte, 32)
	for index := range mailboxKey {
		mailboxKey[index] = byte(index + 1)
	}
	keyPath := writeTestSecret(
		t,
		directory,
		"mailbox-key",
		base64.StdEncoding.EncodeToString(mailboxKey),
	)
	caPath := writeTestCA(t, directory)

	t.Setenv("MAIL_SUITE_STALWART_ENDPOINT", "https://mx.mail-suite.test")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_USERNAME", "admin")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE", passwordPath)
	t.Setenv("MAIL_SUITE_STALWART_MAILBOX_KEY_FILE", keyPath)
	t.Setenv("MAIL_SUITE_STALWART_CA_FILE", caPath)
	t.Setenv("MAIL_SUITE_STALWART_TIMEOUT", "7s")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("加载有效配置失败：%v", err)
	}
	if config.Endpoint.String() != "https://mx.mail-suite.test" ||
		config.AdminPassword != "test-password" || config.RequestTimeout != 7*time.Second ||
		string(config.MailboxKey) != string(mailboxKey) {
		t.Fatalf("加载后的配置与输入不一致：%+v", config)
	}
}

// TestLoadConfigRejectsUnsafeEndpointsAndKeys 验证明文入口和错误长度密钥均阻止 worker 启动。
func TestLoadConfigRejectsUnsafeEndpointsAndKeys(t *testing.T) {
	directory := t.TempDir()
	passwordPath := writeTestSecret(t, directory, "admin-password", "test-password")
	keyPath := writeTestSecret(
		t,
		directory,
		"mailbox-key",
		base64.StdEncoding.EncodeToString(make([]byte, 16)),
	)
	caPath := writeTestCA(t, directory)
	t.Setenv("MAIL_SUITE_STALWART_ENDPOINT", "http://mx.mail-suite.test")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_USERNAME", "admin")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE", passwordPath)
	t.Setenv("MAIL_SUITE_STALWART_MAILBOX_KEY_FILE", keyPath)
	t.Setenv("MAIL_SUITE_STALWART_CA_FILE", caPath)
	t.Setenv("MAIL_SUITE_STALWART_TIMEOUT", "5s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("明文 endpoint 或错误长度密钥不应通过配置校验")
	}

	t.Setenv("MAIL_SUITE_STALWART_ENDPOINT", "https://mx.mail-suite.test")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("错误长度 mailbox key 不应通过配置校验")
	}
}

func TestLoadConfigRequiresCAAndTimeout(t *testing.T) {
	directory := t.TempDir()
	passwordPath := writeTestSecret(t, directory, "admin-password", "test-password")
	keyPath := writeTestSecret(
		t,
		directory,
		"mailbox-key",
		base64.StdEncoding.EncodeToString(make([]byte, 32)),
	)
	t.Setenv("MAIL_SUITE_STALWART_ENDPOINT", "https://mx.mail-suite.test")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_USERNAME", "admin")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE", passwordPath)
	t.Setenv("MAIL_SUITE_STALWART_MAILBOX_KEY_FILE", keyPath)
	t.Setenv("MAIL_SUITE_STALWART_CA_FILE", "")
	t.Setenv("MAIL_SUITE_STALWART_TIMEOUT", "5s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("缺少显式 CA 文件时必须拒绝启动")
	}

	t.Setenv("MAIL_SUITE_STALWART_CA_FILE", writeTestCA(t, directory))
	t.Setenv("MAIL_SUITE_STALWART_TIMEOUT", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("缺少显式请求超时时必须拒绝启动")
	}
}

// TestLoadConfigAcceptsSystemSizedCABundle 防止生产系统 CA 集合被按普通短 secret 的上限误拒绝。
func TestLoadConfigAcceptsSystemSizedCABundle(t *testing.T) {
	directory := t.TempDir()
	passwordPath := writeTestSecret(t, directory, "admin-password", "test-password")
	keyPath := writeTestSecret(
		t,
		directory,
		"mailbox-key",
		base64.StdEncoding.EncodeToString(make([]byte, 32)),
	)
	server := httptest.NewTLSServer(nil)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	server.Close()
	caPath := writeTestSecret(t, directory, "system-sized-ca.pem", strings.Repeat(string(certificate), 32))

	t.Setenv("MAIL_SUITE_STALWART_ENDPOINT", "https://mx.mail-suite.test")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_USERNAME", "admin")
	t.Setenv("MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE", passwordPath)
	t.Setenv("MAIL_SUITE_STALWART_MAILBOX_KEY_FILE", keyPath)
	t.Setenv("MAIL_SUITE_STALWART_CA_FILE", caPath)
	t.Setenv("MAIL_SUITE_STALWART_TIMEOUT", "5s")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("系统规模 CA bundle 应可加载：%v", err)
	}
}

// writeTestSecret 创建权限收敛的测试秘密文件并返回路径。
func writeTestSecret(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试秘密失败：%v", err)
	}
	return path
}

// writeTestCA 把临时 TLS 服务证书写成 adapter 可加载的测试信任文件。
func writeTestCA(t *testing.T, directory string) string {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	certificate := server.Certificate()
	server.Close()
	content := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	return writeTestSecret(t, directory, "stalwart-ca.pem", string(content))
}
