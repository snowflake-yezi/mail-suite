// Package runtimeconfig 读取并校验 API 进程的 OIDC 与会话运行配置。
package runtimeconfig

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	defaultProviderTimeout = 5 * time.Second
	minimumSecretBytes     = 32
)

// Mode 表示 API 进程明确选择的认证运行模式。
type Mode string

const (
	// ModeDisabled 只允许当前回环预览，不注册认证或受保护业务路由。
	ModeDisabled Mode = "disabled"
	// ModeOIDC 要求完整 OIDC 配置并启用服务端认证路由。
	ModeOIDC Mode = "oidc"
)

// Config 保存启动 OIDC adapter 所需的非秘密和已解码秘密配置。
type Config struct {
	// Mode 是部署显式选择的认证运行模式。
	Mode Mode
	// Issuer 是 discovery 和 ID token 必须精确匹配的 HTTPS 签发方。
	Issuer string
	// ClientID 是后端 OIDC client 的稳定标识和预期 audience。
	ClientID string
	// ClientSecret 是 token endpoint 使用的机密客户端凭据。
	ClientSecret string
	// RedirectURI 是 IdP 唯一允许回调的同源 HTTPS 地址。
	RedirectURI string
	// PostLogoutRedirectURI 是 IdP 完成前台退出后唯一允许返回的可信主站根 URL。
	PostLogoutRedirectURI string
	// TrustedOrigin 是所有浏览器状态改变请求必须匹配的主站 origin。
	TrustedOrigin string
	// Scopes 是 authorization request 请求的最小 OIDC scope 集合。
	Scopes []string
	// SigningAlgorithms 是 ID token 唯一允许的非 none 签名算法。
	SigningAlgorithms []string
	// AdminAllowedACR 是管理账号允许满足 MFA 的 acr 值集合。
	AdminAllowedACR []string
	// AdminRequiredAMR 是管理账号必须全部具备的 amr 方法集合。
	AdminRequiredAMR []string
	// SecretPepper 用于不同 purpose 的 state、Cookie、nonce 和 CSRF 摘要。
	SecretPepper []byte
	// FlowEncryptionKey 是 PKCE verifier 信封加密使用的 AES-256 密钥。
	FlowEncryptionKey []byte
	// FlowEncryptionKeyID 是非秘密密钥版本标识，用于未来轮换读取。
	FlowEncryptionKeyID string
	// ProviderTimeout 限制 discovery、JWKS 与 token endpoint 请求。
	ProviderTimeout time.Duration
}

// Load 从 MAIL_SUITE_ 前缀环境变量读取认证配置；任何模式都必须显式声明。
func Load() (Config, error) {
	mode := Mode(strings.TrimSpace(os.Getenv("MAIL_SUITE_AUTH_MODE")))
	if mode == "" {
		return Config{}, errors.New("缺少必需配置 MAIL_SUITE_AUTH_MODE")
	}
	if mode == ModeDisabled {
		return Config{Mode: mode}, nil
	}
	if mode != ModeOIDC {
		return Config{}, errors.New("配置 MAIL_SUITE_AUTH_MODE 只能是 disabled 或 oidc")
	}

	issuer, err := requiredHTTPSURL("MAIL_SUITE_OIDC_ISSUER", false)
	if err != nil {
		return Config{}, err
	}
	redirectURI, err := requiredHTTPSURL("MAIL_SUITE_OIDC_REDIRECT_URI", false)
	if err != nil {
		return Config{}, err
	}
	redirectURL, _ := url.Parse(redirectURI)
	if redirectURL.Path != "/api/v1/auth/callback" || redirectURL.RawQuery != "" {
		return Config{}, errors.New("配置 MAIL_SUITE_OIDC_REDIRECT_URI 必须使用 /api/v1/auth/callback 路径")
	}
	postLogoutRedirectURI, err := requiredHTTPSURL("MAIL_SUITE_OIDC_POST_LOGOUT_REDIRECT_URI", false)
	if err != nil {
		return Config{}, err
	}
	postLogoutRedirectURL, _ := url.Parse(postLogoutRedirectURI)
	if postLogoutRedirectURL.Path != "/" || postLogoutRedirectURL.RawQuery != "" {
		return Config{}, errors.New("配置 MAIL_SUITE_OIDC_POST_LOGOUT_REDIRECT_URI 必须使用根路径 /")
	}
	trustedOrigin, err := requiredHTTPSURL("MAIL_SUITE_AUTH_TRUSTED_ORIGIN", true)
	if err != nil {
		return Config{}, err
	}
	if redirectURL.Scheme+"://"+redirectURL.Host != trustedOrigin {
		return Config{}, errors.New("配置 MAIL_SUITE_OIDC_REDIRECT_URI 必须与 MAIL_SUITE_AUTH_TRUSTED_ORIGIN 同源")
	}
	if postLogoutRedirectURL.Scheme+"://"+postLogoutRedirectURL.Host != trustedOrigin {
		return Config{}, errors.New("配置 MAIL_SUITE_OIDC_POST_LOGOUT_REDIRECT_URI 必须与 MAIL_SUITE_AUTH_TRUSTED_ORIGIN 同源")
	}

	clientID, err := requiredTrimmed("MAIL_SUITE_OIDC_CLIENT_ID")
	if err != nil {
		return Config{}, err
	}
	clientSecret, err := requiredTrimmed("MAIL_SUITE_OIDC_CLIENT_SECRET")
	if err != nil {
		return Config{}, err
	}
	pepper, err := requiredBase64Secret("MAIL_SUITE_AUTH_SECRET_PEPPER", minimumSecretBytes)
	if err != nil {
		return Config{}, err
	}
	flowKey, err := requiredBase64Secret("MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY", minimumSecretBytes)
	if err != nil {
		return Config{}, err
	}
	if len(flowKey) != 32 {
		return Config{}, errors.New("配置 MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY 解码后必须是 32 字节")
	}
	keyID, err := requiredTrimmed("MAIL_SUITE_AUTH_FLOW_ENCRYPTION_KEY_ID")
	if err != nil {
		return Config{}, err
	}

	scopes := uniqueFields(os.Getenv("MAIL_SUITE_OIDC_SCOPES"))
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	if !slices.Contains(scopes, "openid") {
		return Config{}, errors.New("配置 MAIL_SUITE_OIDC_SCOPES 必须包含 openid")
	}
	signingAlgorithms := uniqueFields(os.Getenv("MAIL_SUITE_OIDC_SIGNING_ALGORITHMS"))
	if len(signingAlgorithms) == 0 {
		signingAlgorithms = []string{"RS256"}
	}
	for _, algorithm := range signingAlgorithms {
		if algorithm != "RS256" && algorithm != "PS256" && algorithm != "ES256" {
			return Config{}, errors.New("配置 MAIL_SUITE_OIDC_SIGNING_ALGORITHMS 包含不允许的算法")
		}
	}
	allowedACR := uniqueFields(os.Getenv("MAIL_SUITE_OIDC_ADMIN_ALLOWED_ACR"))
	requiredAMR := uniqueFields(os.Getenv("MAIL_SUITE_OIDC_ADMIN_REQUIRED_AMR"))
	if len(allowedACR) == 0 && len(requiredAMR) == 0 {
		return Config{}, errors.New("管理账号必须配置 MAIL_SUITE_OIDC_ADMIN_ALLOWED_ACR 或 MAIL_SUITE_OIDC_ADMIN_REQUIRED_AMR")
	}
	providerTimeout, err := durationFromEnvironment("MAIL_SUITE_OIDC_PROVIDER_TIMEOUT", defaultProviderTimeout)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Mode:                  mode,
		Issuer:                issuer,
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		PostLogoutRedirectURI: postLogoutRedirectURI,
		TrustedOrigin:         trustedOrigin,
		Scopes:                scopes,
		SigningAlgorithms:     signingAlgorithms,
		AdminAllowedACR:       allowedACR,
		AdminRequiredAMR:      requiredAMR,
		SecretPepper:          pepper,
		FlowEncryptionKey:     flowKey,
		FlowEncryptionKeyID:   keyID,
		ProviderTimeout:       providerTimeout,
	}, nil
}

// requiredHTTPSURL 读取不含凭据、query 或 fragment 的 HTTPS URL，可选限制为纯 origin。
func requiredHTTPSURL(name string, originOnly bool) (string, error) {
	raw, err := requiredTrimmed(name)
	if err != nil {
		return "", err
	}
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("配置 %s 必须是无凭据的 HTTPS URL", name)
	}
	if originOnly && (parsed.Path != "" || parsed.RawQuery != "") {
		return "", fmt.Errorf("配置 %s 必须是无路径和 query 的 HTTPS origin", name)
	}
	if !originOnly && parsed.RawQuery != "" {
		return "", fmt.Errorf("配置 %s 不得包含 query", name)
	}
	return raw, nil
}

// requiredTrimmed 读取非空且不存在首尾空白的单值配置。
func requiredTrimmed(name string) (string, error) {
	raw := os.Getenv(name)
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("缺少必需配置 %s", name)
	}
	if value != raw {
		return "", fmt.Errorf("配置 %s 不得包含首尾空白", name)
	}
	return value, nil
}

// requiredBase64Secret 解码标准 Base64 secret，并确保错误不回显输入。
func requiredBase64Secret(name string, minimumBytes int) ([]byte, error) {
	raw, err := requiredTrimmed(name)
	if err != nil {
		return nil, err
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(raw)
	if decodeErr != nil || len(decoded) < minimumBytes {
		return nil, fmt.Errorf("配置 %s 必须是解码后至少 %d 字节的标准 Base64", name, minimumBytes)
	}
	return decoded, nil
}

// uniqueFields 解析空白或逗号分隔列表，保留首次出现顺序并移除重复值。
func uniqueFields(raw string) []string {
	fields := strings.FieldsFunc(raw, func(character rune) bool {
		return character == ',' || character == ' ' || character == '\t' || character == '\r' || character == '\n'
	})
	values := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if _, found := seen[field]; found {
			continue
		}
		seen[field] = struct{}{}
		values = append(values, field)
	}
	return values
}

// durationFromEnvironment 读取正数 provider 超时，不回显无效原值。
func durationFromEnvironment(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 || value > 30*time.Second {
		return 0, fmt.Errorf("配置 %s 必须是大于零且不超过 30 秒的时长", name)
	}
	return value, nil
}
