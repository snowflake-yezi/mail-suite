package runtimeconfig

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresExplicitMode(t *testing.T) {
	t.Setenv("MAIL_SUITE_AUTH_MODE", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAIL_SUITE_AUTH_MODE") {
		t.Fatalf("缺少显式认证模式应拒绝启动，实际为 %v", err)
	}

	t.Setenv("MAIL_SUITE_AUTH_MODE", string(ModeDisabled))
	config, err := Load()
	if err != nil || config.Mode != ModeDisabled {
		t.Fatalf("disabled 模式不应要求 OIDC secret：config=%+v err=%v", config, err)
	}
}

func TestLoadAcceptsCompleteOIDCConfiguration(t *testing.T) {
	setValidOIDCEnvironment(t)
	t.Setenv("MAIL_SUITE_OIDC_SCOPES", "openid,profile openid")
	t.Setenv("MAIL_SUITE_OIDC_SIGNING_ALGORITHMS", "RS256,PS256")
	t.Setenv("MAIL_SUITE_OIDC_PROVIDER_TIMEOUT", "3s")

	config, err := Load()
	if err != nil {
		t.Fatalf("完整 OIDC 配置应有效：%v", err)
	}
	if config.Mode != ModeOIDC || config.ProviderTimeout != 3*time.Second {
		t.Fatalf("OIDC 模式或超时读取错误：%+v", config)
	}
	if strings.Join(config.Scopes, ",") != "openid,profile" {
		t.Fatalf("OIDC scope 应去重并保序：%v", config.Scopes)
	}
}

func TestLoadRejectsInsecureOrCrossOriginOIDCURLs(t *testing.T) {
	setValidOIDCEnvironment(t)
	t.Setenv("MAIL_SUITE_OIDC_ISSUER", "http://idp.example.test/realms/mail-suite")
	if _, err := Load(); err == nil {
		t.Fatal("HTTP issuer 不得进入 OIDC 模式")
	}

	setValidOIDCEnvironment(t)
	t.Setenv("MAIL_SUITE_OIDC_REDIRECT_URI", "https://other.example.test/api/v1/auth/callback")
	if _, err := Load(); err == nil {
		t.Fatal("跨 origin callback 不得进入 OIDC 模式")
	}
}

func TestLoadHidesInvalidSecretValues(t *testing.T) {
	setValidOIDCEnvironment(t)
	const secretLikeValue = "not-base64-secret-value"
	t.Setenv("MAIL_SUITE_AUTH_SECRET_PEPPER", secretLikeValue)
	_, err := Load()
	if err == nil || strings.Contains(err.Error(), secretLikeValue) {
		t.Fatalf("无效 secret 错误必须脱敏，实际为 %v", err)
	}
}

// setValidOIDCEnvironment 为每个配置测试设置完整且不含真实凭据的假值。
func setValidOIDCEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MAIL_SUITE_AUTH_MODE", string(ModeOIDC))
	t.Setenv("MAIL_SUITE_OIDC_ISSUER", "https://idp.example.test/realms/mail-suite")
	t.Setenv("MAIL_SUITE_OIDC_CLIENT_ID", "mail-suite-test")
	t.Setenv("MAIL_SUITE_OIDC_CLIENT_SECRET", "test-client-secret")
	t.Setenv("MAIL_SUITE_OIDC_REDIRECT_URI", "https://mail.example.test/api/v1/auth/callback")
	t.Setenv("MAIL_SUITE_AUTH_TRUSTED_ORIGIN", "https://mail.example.test")
	t.Setenv("MAIL_SUITE_OIDC_SCOPES", "openid profile email")
	t.Setenv("MAIL_SUITE_OIDC_SIGNING_ALGORITHMS", "RS256")
	t.Setenv("MAIL_SUITE_OIDC_ADMIN_ALLOWED_ACR", "urn:example:mfa")
	t.Setenv("MAIL_SUITE_OIDC_ADMIN_REQUIRED_AMR", "")
	t.Setenv("MAIL_SUITE_AUTH_SECRET_PEPPER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, 32)))
	t.Setenv("MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x66}, 32)))
	t.Setenv("MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY_ID", "test-v1")
	t.Setenv("MAIL_SUITE_OIDC_PROVIDER_TIMEOUT", "5s")
}
