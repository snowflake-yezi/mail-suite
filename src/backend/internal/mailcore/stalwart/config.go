// Package stalwart 通过锁定版本的 Stalwart JMAP 管理能力实现邮件内核适配器。
package stalwart

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"
)

const (
	maximumRequestTimeout = 30 * time.Second
	maximumSecretBytes    = 4096
	maximumCABundleBytes  = 1 << 20
)

// Config 保存 Stalwart adapter 的已校验连接、认证和派生密钥配置。
type Config struct {
	// Endpoint 是只允许 HTTPS 且不包含路径的 Stalwart 内部服务 origin。
	Endpoint *url.URL
	// AdminUsername 是仅用于内部管理 JMAP 的稳定管理员名称。
	AdminUsername string
	// AdminPassword 是从受限文件读取且不得写入日志的管理密码。
	AdminPassword string
	// MailboxKey 用于派生每个测试邮箱独立的内部 JMAP 凭据。
	MailboxKey []byte
	// RootCAs 是必须显式加载的 Stalwart 内部 TLS 信任集合。
	RootCAs *x509.CertPool
	// RequestTimeout 限制单次 discovery、query、get 或 set 请求时长。
	RequestTimeout time.Duration
}

// LoadConfig 从环境和受限文件加载真实 Stalwart adapter 配置。
func LoadConfig() (Config, error) {
	endpoint, err := parseEndpoint(os.Getenv("MAIL_SUITE_STALWART_ENDPOINT"))
	if err != nil {
		return Config{}, err
	}

	username := strings.TrimSpace(os.Getenv("MAIL_SUITE_STALWART_ADMIN_USERNAME"))
	if username == "" || len(username) > 256 || strings.Contains(username, ":") ||
		strings.IndexFunc(username, unicode.IsControl) >= 0 {
		return Config{}, errors.New("配置 MAIL_SUITE_STALWART_ADMIN_USERNAME 无效")
	}

	password, err := readRestrictedSecret(
		"MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE",
		os.Getenv("MAIL_SUITE_STALWART_ADMIN_PASSWORD_FILE"),
	)
	if err != nil {
		return Config{}, err
	}
	encodedMailboxKey, err := readRestrictedSecret(
		"MAIL_SUITE_STALWART_MAILBOX_KEY_FILE",
		os.Getenv("MAIL_SUITE_STALWART_MAILBOX_KEY_FILE"),
	)
	if err != nil {
		return Config{}, err
	}
	mailboxKey, err := base64.StdEncoding.DecodeString(encodedMailboxKey)
	if err != nil || len(mailboxKey) != 32 {
		return Config{}, errors.New("配置 MAIL_SUITE_STALWART_MAILBOX_KEY_FILE 必须包含 32 字节 Base64 密钥")
	}

	rawTimeout := strings.TrimSpace(os.Getenv("MAIL_SUITE_STALWART_TIMEOUT"))
	if rawTimeout == "" {
		return Config{}, errors.New("缺少必需配置 MAIL_SUITE_STALWART_TIMEOUT")
	}
	requestTimeout, err := time.ParseDuration(rawTimeout)
	if err != nil || requestTimeout <= 0 || requestTimeout > maximumRequestTimeout {
		return Config{}, errors.New("配置 MAIL_SUITE_STALWART_TIMEOUT 必须是 30 秒内的正数时长")
	}

	caPath := strings.TrimSpace(os.Getenv("MAIL_SUITE_STALWART_CA_FILE"))
	if caPath == "" {
		return Config{}, errors.New("缺少必需配置 MAIL_SUITE_STALWART_CA_FILE")
	}
	rootCAs, err := loadRootCAs(caPath)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Endpoint:       endpoint,
		AdminUsername:  username,
		AdminPassword:  password,
		MailboxKey:     mailboxKey,
		RootCAs:        rootCAs,
		RequestTimeout: requestTimeout,
	}, nil
}

// parseEndpoint 要求内部管理入口使用没有歧义的 HTTPS origin。
func parseEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("配置 MAIL_SUITE_STALWART_ENDPOINT 必须是无路径的 HTTPS origin")
	}
	parsed.Path = ""
	return parsed, nil
}

// readRestrictedSecret 从单个受限文件读取短秘密，并拒绝 Linux 上的组或其他用户权限。
func readRestrictedSecret(environmentName, rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", fmt.Errorf("缺少必需配置 %s", environmentName)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("配置 %s 指向的秘密文件不可读", environmentName)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("配置 %s 指向的秘密文件权限过宽", environmentName)
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) > maximumSecretBytes {
		return "", fmt.Errorf("配置 %s 指向的秘密文件不可读", environmentName)
	}
	secret := strings.TrimSpace(string(content))
	if secret == "" || strings.IndexFunc(secret, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("配置 %s 指向的秘密无效", environmentName)
	}
	return secret, nil
}

// loadRootCAs 在有界读取后把部署指定的 PEM CA 合并进系统信任集合。
func loadRootCAs(path string) (*x509.CertPool, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumCABundleBytes {
		return nil, errors.New("配置 MAIL_SUITE_STALWART_CA_FILE 指向的 CA 文件不可读")
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) == 0 || len(content) > maximumCABundleBytes {
		return nil, errors.New("配置 MAIL_SUITE_STALWART_CA_FILE 指向的 CA 文件不可读")
	}
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	if !rootCAs.AppendCertsFromPEM(content) {
		return nil, errors.New("配置 MAIL_SUITE_STALWART_CA_FILE 不包含有效 PEM 证书")
	}
	return rootCAs, nil
}
