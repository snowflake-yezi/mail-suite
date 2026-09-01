package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const pkceAssociatedDataPrefix = "mail-suite:pkce:"

// PKCECipher 使用带密钥版本的 AES-256-GCM 信封保护需要在 callback 读取的 verifier。
type PKCECipher struct {
	currentKeyID string
	keys         map[string]cipher.AEAD
	random       io.Reader
}

// NewPKCECipher 创建支持读取历史密钥的 PKCE 密文边界；当前写入密钥必须存在且全部为 32 字节。
func NewPKCECipher(currentKeyID string, keys map[string][]byte) (*PKCECipher, error) {
	if currentKeyID == "" || currentKeyID != strings.TrimSpace(currentKeyID) {
		return nil, errors.New("PKCE 当前加密密钥标识不能为空或包含首尾空白")
	}
	if len(keys) == 0 {
		return nil, errors.New("PKCE 加密密钥集合不能为空")
	}
	aeadKeys := make(map[string]cipher.AEAD, len(keys))
	for keyID, key := range keys {
		if keyID == "" || keyID != strings.TrimSpace(keyID) || len(key) != 32 {
			return nil, errors.New("PKCE 加密密钥必须使用非空标识和 32 字节 AES-256 密钥")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, errors.New("PKCE 加密密钥初始化失败")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errors.New("PKCE AEAD 初始化失败")
		}
		aeadKeys[keyID] = aead
	}
	if _, found := aeadKeys[currentKeyID]; !found {
		return nil, errors.New("PKCE 当前加密密钥标识不存在于密钥集合")
	}
	return &PKCECipher{currentKeyID: currentKeyID, keys: aeadKeys, random: rand.Reader}, nil
}

// Encrypt 使用当前密钥和随机 nonce 加密合法 PKCE verifier，并返回非秘密密钥版本。
func (cipherBox *PKCECipher) Encrypt(verifier string) ([]byte, string, error) {
	if !validPKCEVerifier(verifier) {
		return nil, "", NewError(ErrorCodeInvalidRequest)
	}
	aead := cipherBox.keys[cipherBox.currentKeyID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(cipherBox.random, nonce); err != nil {
		return nil, "", NewError(ErrorCodePersistenceUnavailable)
	}
	ciphertext := aead.Seal(
		append([]byte(nil), nonce...),
		nonce,
		[]byte(verifier),
		[]byte(pkceAssociatedDataPrefix+cipherBox.currentKeyID),
	)
	return ciphertext, cipherBox.currentKeyID, nil
}

// Decrypt 按持久化密钥版本恢复 verifier；未知密钥、篡改或格式损坏统一视为登录流程失效。
func (cipherBox *PKCECipher) Decrypt(ciphertext []byte, keyID string) (string, error) {
	aead, found := cipherBox.keys[keyID]
	if !found || len(ciphertext) <= aeadNonceSize(aead) {
		return "", NewError(ErrorCodeAuthFlowInvalid)
	}
	nonceSize := aead.NonceSize()
	plaintext, err := aead.Open(
		nil,
		ciphertext[:nonceSize],
		ciphertext[nonceSize:],
		[]byte(pkceAssociatedDataPrefix+keyID),
	)
	if err != nil || !validPKCEVerifier(string(plaintext)) {
		return "", NewError(ErrorCodeAuthFlowInvalid)
	}
	return string(plaintext), nil
}

// PKCEChallenge 根据 RFC 7636 S256 规则生成不带填充的 URL 安全 challenge。
func PKCEChallenge(verifier string) (string, error) {
	if !validPKCEVerifier(verifier) {
		return "", NewError(ErrorCodeInvalidRequest)
	}
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// validPKCEVerifier 只接受 RFC 7636 规定的 43 到 128 个 unreserved ASCII 字符。
func validPKCEVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("-._~", character) {
			continue
		}
		return false
	}
	return true
}

// aeadNonceSize 防御 nil AEAD，正常构造路径始终返回 GCM nonce 长度。
func aeadNonceSize(aead cipher.AEAD) int {
	if aead == nil {
		return 0
	}
	return aead.NonceSize()
}

// String 只提供不含密钥或密文的调试标识。
func (cipherBox *PKCECipher) String() string {
	return fmt.Sprintf("PKCECipher(current_key_id=%s,key_count=%d)", cipherBox.currentKeyID, len(cipherBox.keys))
}
