package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

const (
	minimumPepperBytes = 32
	opaqueTokenBytes   = 32
)

// SecretPurpose 隔离同一服务端 pepper 对不同认证秘密执行的摘要域。
type SecretPurpose string

const (
	// SecretPurposeSessionCookie 用于服务端会话 Cookie 摘要。
	SecretPurposeSessionCookie SecretPurpose = "session-cookie"
	// SecretPurposeAuthFlowState 用于 OIDC state 摘要。
	SecretPurposeAuthFlowState SecretPurpose = "auth-flow-state"
	// SecretPurposeAuthFlowCookie 用于临时浏览器关联 Cookie 摘要。
	SecretPurposeAuthFlowCookie SecretPurpose = "auth-flow-cookie"
	// SecretPurposeOIDCNonce 用于 OIDC nonce 摘要。
	SecretPurposeOIDCNonce SecretPurpose = "oidc-nonce"
	// SecretPurposeCSRFToken 用于会话绑定 CSRF nonce 摘要。
	SecretPurposeCSRFToken SecretPurpose = "csrf-token"
	// SecretPurposeSource 用于请求来源信息的审计摘要。
	SecretPurposeSource         SecretPurpose = "audit-source"
	secretPurposeCSRFDerivation SecretPurpose = "csrf-derivation"
)

// SecretHasher 使用独立 purpose 和服务端 pepper 生成不可逆 SHA-256 HMAC 摘要。
type SecretHasher struct {
	pepper []byte
}

// NewSecretHasher 创建认证秘密摘要器；pepper 少于 256 bit 时拒绝启动。
func NewSecretHasher(pepper []byte) (*SecretHasher, error) {
	if len(pepper) < minimumPepperBytes {
		return nil, errors.New("认证摘要 pepper 至少需要 32 字节")
	}
	pepperCopy := append([]byte(nil), pepper...)
	return &SecretHasher{pepper: pepperCopy}, nil
}

// Digest 对指定用途和秘密值生成固定长度 HMAC，不保留调用方输入。
func (hasher *SecretHasher) Digest(purpose SecretPurpose, secret string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, hasher.pepper)
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(secret))
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest
}

// DeriveCSRFToken 从当前不透明会话值派生可重复返回的 CSRF nonce。
func (hasher *SecretHasher) DeriveCSRFToken(sessionToken string) string {
	digest := hasher.Digest(secretPurposeCSRFDerivation, sessionToken)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// NewOpaqueToken 使用 CSPRNG 生成至少 256 bit 熵的 URL 安全不透明值。
func NewOpaqueToken() (string, error) {
	buffer := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", errors.New("认证随机值生成失败")
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
