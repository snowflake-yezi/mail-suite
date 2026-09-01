package identity

import (
	"bytes"
	"testing"
)

const rfc7636Verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

func TestPKCEChallengeMatchesRFC7636Example(t *testing.T) {
	challenge, err := PKCEChallenge(rfc7636Verifier)
	if err != nil {
		t.Fatalf("生成 PKCE challenge 失败：%v", err)
	}
	if challenge != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("PKCE S256 challenge 与 RFC 7636 不一致：%q", challenge)
	}
}

func TestPKCECipherEncryptsAndReadsHistoricalKey(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x11}, 32)
	newKey := bytes.Repeat([]byte{0x22}, 32)
	oldCipher, err := NewPKCECipher("old", map[string][]byte{"old": oldKey})
	if err != nil {
		t.Fatalf("创建旧 PKCE 密钥失败：%v", err)
	}
	ciphertext, keyID, err := oldCipher.Encrypt(rfc7636Verifier)
	if err != nil {
		t.Fatalf("加密 PKCE verifier 失败：%v", err)
	}

	rotatedCipher, err := NewPKCECipher("new", map[string][]byte{"old": oldKey, "new": newKey})
	if err != nil {
		t.Fatalf("创建轮换后 PKCE 密钥失败：%v", err)
	}
	plaintext, err := rotatedCipher.Decrypt(ciphertext, keyID)
	if err != nil || plaintext != rfc7636Verifier {
		t.Fatalf("历史密钥解密失败：plaintext=%q err=%v", plaintext, err)
	}
}

func TestPKCECipherRejectsTamperingUnknownKeyAndInvalidVerifier(t *testing.T) {
	cipherBox, err := NewPKCECipher("v1", map[string][]byte{
		"v1": bytes.Repeat([]byte{0x33}, 32),
	})
	if err != nil {
		t.Fatalf("创建 PKCE 密钥失败：%v", err)
	}
	if _, _, err = cipherBox.Encrypt("too-short"); !HasErrorCode(err, ErrorCodeInvalidRequest) {
		t.Fatalf("非法 verifier 应被拒绝，实际为 %v", err)
	}
	ciphertext, _, err := cipherBox.Encrypt(rfc7636Verifier)
	if err != nil {
		t.Fatalf("加密 PKCE verifier 失败：%v", err)
	}
	ciphertext[len(ciphertext)-1] ^= 0xff
	if _, err = cipherBox.Decrypt(ciphertext, "v1"); !HasErrorCode(err, ErrorCodeAuthFlowInvalid) {
		t.Fatalf("篡改密文应统一映射为流程失效，实际为 %v", err)
	}
	if _, err = cipherBox.Decrypt(ciphertext, "missing"); !HasErrorCode(err, ErrorCodeAuthFlowInvalid) {
		t.Fatalf("未知密钥应统一映射为流程失效，实际为 %v", err)
	}
}

func TestPKCECipherRejectsUnsafeKeyConfiguration(t *testing.T) {
	if _, err := NewPKCECipher("v1", map[string][]byte{"v1": bytes.Repeat([]byte{0x44}, 31)}); err == nil {
		t.Fatal("非 AES-256 密钥不得用于 PKCE verifier")
	}
	if _, err := NewPKCECipher("missing", map[string][]byte{"v1": bytes.Repeat([]byte{0x44}, 32)}); err == nil {
		t.Fatal("当前写入密钥不存在时必须拒绝启动")
	}
}
