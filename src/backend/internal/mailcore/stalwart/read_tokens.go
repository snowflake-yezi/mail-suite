package stalwart

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

const (
	cursorLifetime    = 10 * time.Minute
	referenceLifetime = 15 * time.Minute
	maximumTokenBytes = 4096
)

// readToken 是经认证加密的内部 JMAP 标识与分页状态。
type readToken struct {
	Version           int       `json:"v"`
	Purpose           string    `json:"p"`
	MailboxID         uuid.UUID `json:"m"`
	DomainID          uuid.UUID `json:"d"`
	Revision          int64     `json:"r"`
	ConfigurationHash [32]byte  `json:"h"`
	ExpiresAt         int64     `json:"e"`
	MessageID         string    `json:"i,omitempty"`
	BlobID            string    `json:"b,omitempty"`
	QueryState        string    `json:"q,omitempty"`
	Position          int       `json:"n,omitempty"`
}

// newReadToken 填充不可变邮箱绑定和用途到期时间。
func (adapter *Adapter) newReadToken(scope mailcore.ReadScope, purpose string, lifetime time.Duration) readToken {
	return readToken{
		Version:           1,
		Purpose:           purpose,
		MailboxID:         scope.MailboxID,
		DomainID:          scope.DomainID,
		Revision:          scope.Revision,
		ConfigurationHash: scope.ConfigurationHash,
		ExpiresAt:         adapter.now().Add(lifetime).Unix(),
	}
}

// tokenCipher 为邮件读取令牌派生与开通凭据用途隔离的认证加密密钥。
func (adapter *Adapter) tokenCipher() (cipher.AEAD, error) {
	derivation := hmac.New(sha256.New, adapter.mailboxKey)
	_, _ = derivation.Write([]byte("mail-suite:mail-read-token:v1"))
	block, err := aes.NewCipher(derivation.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealReadToken 加密内部标识，公开令牌不会暴露明文 JMAP ID。
func (adapter *Adapter) sealReadToken(value readToken) (string, error) {
	plaintext, err := json.Marshal(value)
	if err != nil {
		return "", protocolError()
	}
	aead, err := adapter.tokenCipher()
	if err != nil {
		return "", protocolError()
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", protocolError()
	}
	sealed := aead.Seal(nonce, nonce, plaintext, nil)
	if len(sealed) > maximumTokenBytes {
		return "", protocolError()
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openReadToken 验证令牌完整性、用途、时效及准确的控制面邮箱配置。
func (adapter *Adapter) openReadToken(raw string, scope mailcore.ReadScope, purpose string) (readToken, error) {
	invalid := tokenError(purpose)
	if raw == "" || len(raw) > base64.RawURLEncoding.EncodedLen(maximumTokenBytes) {
		return readToken{}, invalid
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(encoded) > maximumTokenBytes {
		return readToken{}, invalid
	}
	aead, err := adapter.tokenCipher()
	if err != nil || len(encoded) < aead.NonceSize()+aead.Overhead() {
		return readToken{}, invalid
	}
	plaintext, err := aead.Open(nil, encoded[:aead.NonceSize()], encoded[aead.NonceSize():], nil)
	if err != nil {
		return readToken{}, invalid
	}
	var value readToken
	if json.Unmarshal(plaintext, &value) != nil || value.Version != 1 || value.Purpose != purpose ||
		value.MailboxID != scope.MailboxID || value.DomainID != scope.DomainID ||
		value.Revision != scope.Revision || value.ConfigurationHash != scope.ConfigurationHash ||
		adapter.now().Unix() >= value.ExpiresAt {
		return readToken{}, invalid
	}
	return value, nil
}

// tokenError 将分页和资源引用的无效令牌映射到不同稳定错误码。
func tokenError(purpose string) error {
	if purpose == "cursor" {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeCursorInvalid)
	}
	return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeReferenceInvalid)
}
