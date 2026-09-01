package identity

import (
	"bytes"
	"testing"
	"time"
)

func TestSecretHasherSeparatesPurposesAndDerivesStableCSRFToken(t *testing.T) {
	pepper := bytes.Repeat([]byte{0x5a}, 32)
	hasher, err := NewSecretHasher(pepper)
	if err != nil {
		t.Fatalf("创建认证摘要器失败：%v", err)
	}

	sessionDigest := hasher.Digest(SecretPurposeSessionCookie, "same-secret")
	stateDigest := hasher.Digest(SecretPurposeAuthFlowState, "same-secret")
	if sessionDigest == stateDigest {
		t.Fatal("不同认证用途不得产生相同摘要")
	}
	firstCSRF := hasher.DeriveCSRFToken("session-value")
	secondCSRF := hasher.DeriveCSRFToken("session-value")
	if firstCSRF == "" || firstCSRF != secondCSRF {
		t.Fatal("同一会话必须派生稳定且非空的 CSRF nonce")
	}
	csrfDigest := hasher.Digest(SecretPurposeCSRFToken, firstCSRF)
	if csrfDigest == sessionDigest {
		t.Fatal("CSRF 摘要不得与会话 Cookie 摘要复用")
	}
}

func TestSecretHasherRejectsShortPepperAndOpaqueTokenHasEnoughEntropy(t *testing.T) {
	if _, err := NewSecretHasher(bytes.Repeat([]byte{0x01}, 31)); err == nil {
		t.Fatal("少于 32 字节的 pepper 必须被拒绝")
	}
	token, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("生成不透明认证随机值失败：%v", err)
	}
	if len(token) != 43 {
		t.Fatalf("256 bit RawURL token 长度应为 43，实际为 %d", len(token))
	}
}

func TestSessionExpiresAtUsesEarlierBoundary(t *testing.T) {
	now := time.Now().UTC()
	session := Session{
		IdleExpiresAt:     now.Add(DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: now.Add(DefaultSessionAbsoluteTimeout),
	}
	if !session.ExpiresAt().Equal(session.IdleExpiresAt) {
		t.Fatal("会话有效期必须使用空闲与绝对过期中的较早值")
	}
}

func TestStableDomainValuesRejectUnknownInput(t *testing.T) {
	if AccountType("owner").Valid() {
		t.Fatal("未知账号类型不得进入产品会话")
	}
	if RevocationReason("raw-provider-error").Valid() {
		t.Fatal("任意外部错误不得直接作为撤销原因")
	}
	if !HasErrorCode(NewError(ErrorCodeAuthRequired), ErrorCodeAuthRequired) {
		t.Fatal("认证错误码应可通过错误链稳定识别")
	}
}

func TestNormalizeReturnPathAcceptsOnlyCanonicalProtectedPortalPaths(t *testing.T) {
	tests := map[string]string{
		"/mail":                             "/mail",
		"/mail/inbox":                       "/mail/inbox",
		"/admin/overview?filter=active":     "/admin/overview?filter=active",
		"/mail/search?q=hello%20mail":       "/mail/search?q=hello%20mail",
		"/admin/domains/%E4%BE%8B%E5%AD%90": "/admin/domains/%E4%BE%8B%E5%AD%90",
	}
	for candidate, expected := range tests {
		normalized, err := NormalizeReturnPath(candidate)
		if err != nil || normalized != expected {
			t.Fatalf("合法返回路径规范化失败：candidate=%q normalized=%q err=%v", candidate, normalized, err)
		}
	}
}

func TestNormalizeReturnPathRejectsCrossOriginEncodingAndControlAmbiguity(t *testing.T) {
	invalid := []string{
		"",
		"mail/inbox",
		"//example.test/mail",
		"https://example.test/mail",
		"/login",
		"/mail/../admin/overview",
		"/mail//inbox",
		"/mail/%2f%2fevil",
		"/mail/%252f%252fevil",
		"/mail/%5cevil",
		"/mail/inbox?next=%250aadmin",
		"/mail/inbox?next=%0aadmin",
		"/mail\\admin",
		"/mail\x00admin",
	}
	for _, candidate := range invalid {
		if normalized, err := NormalizeReturnPath(candidate); err == nil {
			t.Fatalf("危险返回路径被接受：candidate=%q normalized=%q", candidate, normalized)
		} else if !HasErrorCode(err, ErrorCodeInvalidReturnTo) {
			t.Fatalf("危险返回路径应使用稳定错误码：candidate=%q err=%v", candidate, err)
		}
	}
}
