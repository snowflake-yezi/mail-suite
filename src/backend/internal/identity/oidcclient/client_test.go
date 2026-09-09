package oidcclient

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/runtimeconfig"
)

// testProvider 保存本地 TLS OIDC provider 的签名材料和可控响应。
type testProvider struct {
	server             *httptest.Server
	privateKey         *rsa.PrivateKey
	claims             map[string]any
	endSessionEndpoint string
	tokenStatus        int
	tokenDelay         time.Duration
	mutex              sync.Mutex
	lastVerifier       string
}

func TestClientPerformsDiscoveryAuthorizationAndVerifiedExchange(t *testing.T) {
	provider := newTestProvider(t)
	client := newTestClient(t, provider, 2*time.Second)

	authorizationURL, err := client.AuthorizationURL("test-state", "test-nonce", "test-challenge")
	if err != nil {
		t.Fatalf("创建授权 URL 失败：%v", err)
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("解析授权 URL 失败：%v", err)
	}
	query := parsed.Query()
	if parsed.String() == "" || query.Get("state") != "test-state" || query.Get("nonce") != "test-nonce" ||
		query.Get("code_challenge") != "test-challenge" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("授权 URL 缺少一次性安全参数：%s", authorizationURL)
	}

	externalIdentity, err := client.Exchange(context.Background(), "valid-code", oidcTestVerifier)
	if err != nil {
		t.Fatalf("OIDC code exchange 失败：%v", err)
	}
	if externalIdentity.Issuer != provider.server.URL || externalIdentity.Subject != "subject-1" ||
		externalIdentity.Nonce != "test-nonce" || externalIdentity.SessionID != "session-1" {
		t.Fatalf("OIDC 身份提取错误：%+v", externalIdentity)
	}
	if provider.observedVerifier() != oidcTestVerifier {
		t.Fatal("token endpoint 未收到原始 PKCE verifier")
	}
	logoutURL, err := url.Parse(client.ProviderLogoutURL())
	if err != nil || logoutURL.Path != "/logout" || logoutURL.Query().Get("client_id") != "mail-suite-test" ||
		logoutURL.Query().Get("post_logout_redirect_uri") != "https://mail.example.test/" || len(logoutURL.Query()) != 2 {
		t.Fatalf("provider 前台退出 URL 构造错误：%q err=%v", client.ProviderLogoutURL(), err)
	}
}

func TestBuildProviderLogoutURLRejectsUnsafeRedirect(t *testing.T) {
	for _, redirect := range []string{
		"",
		"http://mail.example.test/",
		"https://user@mail.example.test/",
		"https://mail.example.test/path",
		"https://mail.example.test/?next=/mail",
		"https://mail.example.test/#fragment",
	} {
		if _, err := buildProviderLogoutURL("https://idp.example.test/logout", "client", redirect); err == nil {
			t.Fatalf("OIDC adapter 必须拒绝不安全回跳：%q", redirect)
		}
	}
}

func TestClientRejectsUnsafeEndSessionEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint func(*testProvider) string
	}{
		{name: "missing", endpoint: func(*testProvider) string { return "" }},
		{name: "http", endpoint: func(*testProvider) string { return "http://idp.example.test/logout" }},
		{name: "userinfo", endpoint: func(*testProvider) string { return "https://user@idp.example.test/logout" }},
		{name: "query", endpoint: func(provider *testProvider) string { return provider.server.URL + "/logout?fixed=value" }},
		{name: "fragment", endpoint: func(provider *testProvider) string { return provider.server.URL + "/logout#fragment" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestProvider(t)
			provider.endSessionEndpoint = test.endpoint(provider)
			_, err := newWithHTTPClient(context.Background(), runtimeconfig.Config{
				Mode:                  runtimeconfig.ModeOIDC,
				Issuer:                provider.server.URL,
				ClientID:              "mail-suite-test",
				ClientSecret:          "test-client-secret",
				RedirectURI:           "https://mail.example.test/api/v1/auth/callback",
				PostLogoutRedirectURI: "https://mail.example.test/",
				Scopes:                []string{"openid", "profile"},
				SigningAlgorithms:     []string{"RS256"},
				ProviderTimeout:       2 * time.Second,
			}, provider.server.Client())
			if err == nil {
				t.Fatalf("不安全 end-session endpoint 必须拒绝：%q", provider.endSessionEndpoint)
			}
		})
	}
}

func TestClientRejectsWrongAuthorizedPartyAndFutureIssuedAt(t *testing.T) {
	tests := []struct {
		name  string
		claim string
		value any
	}{
		{name: "wrong azp", claim: "azp", value: "other-client"},
		{name: "future iat", claim: "iat", value: time.Now().Add(2 * time.Minute).Unix()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestProvider(t)
			provider.claims[test.claim] = test.value
			client := newTestClient(t, provider, 2*time.Second)
			_, err := client.Exchange(context.Background(), "valid-code", oidcTestVerifier)
			if !identity.HasErrorCode(err, identity.ErrorCodeAuthFlowInvalid) {
				t.Fatalf("无效 %s 应拒绝 callback，实际为 %v", test.claim, err)
			}
		})
	}
}

func TestClientClassifiesProviderRejectionAndTimeout(t *testing.T) {
	provider := newTestProvider(t)
	provider.tokenStatus = http.StatusBadRequest
	client := newTestClient(t, provider, 2*time.Second)
	_, err := client.Exchange(context.Background(), "rejected-code", oidcTestVerifier)
	if !identity.HasErrorCode(err, identity.ErrorCodeAuthFlowInvalid) {
		t.Fatalf("provider 明确拒绝 code 应视为无效流程，实际为 %v", err)
	}

	timeoutProvider := newTestProvider(t)
	timeoutProvider.tokenDelay = 200 * time.Millisecond
	timeoutClient := newTestClient(t, timeoutProvider, 2*time.Second)
	timeoutClient.httpClient.Timeout = 30 * time.Millisecond
	_, err = timeoutClient.Exchange(context.Background(), "valid-code", oidcTestVerifier)
	if !identity.HasErrorCode(err, identity.ErrorCodePersistenceUnavailable) {
		t.Fatalf("provider 超时应视为认证服务不可用，实际为 %v", err)
	}
}

func TestBoundedProviderBodyRejectsContentOverLimit(t *testing.T) {
	body := &boundedReadCloser{
		body:      io.NopCloser(&repeatingReader{remaining: 9}),
		remaining: 8,
	}
	content, err := io.ReadAll(body)
	if !errors.Is(err, errProviderResponseTooLarge) || len(content) != 8 {
		t.Fatalf("超限 provider 响应应在边界处失败：bytes=%d err=%v", len(content), err)
	}
}

const oidcTestVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

// repeatingReader 生成指定长度的无敏感测试响应体。
type repeatingReader struct {
	remaining int
}

// Read 按调用方缓冲区逐步生成固定字节。
func (reader *repeatingReader) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if count > reader.remaining {
		count = reader.remaining
	}
	for index := range count {
		buffer[index] = 'x'
	}
	reader.remaining -= count
	return count, nil
}

// newTestProvider 创建提供 discovery、token 和 JWKS 的本地 TLS OIDC fixture。
func newTestProvider(t *testing.T) *testProvider {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 OIDC 测试签名密钥失败：%v", err)
	}
	provider := &testProvider{privateKey: privateKey}
	provider.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(t, writer, map[string]any{
				"issuer":                                provider.server.URL,
				"authorization_endpoint":                provider.server.URL + "/authorize",
				"token_endpoint":                        provider.server.URL + "/token",
				"jwks_uri":                              provider.server.URL + "/jwks",
				"end_session_endpoint":                  provider.endSessionEndpoint,
				"response_types_supported":              []string{"code"},
				"subject_types_supported":               []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
				"code_challenge_methods_supported":      []string{"S256"},
			})
		case "/token":
			if provider.tokenDelay > 0 {
				time.Sleep(provider.tokenDelay)
			}
			if provider.tokenStatus != 0 {
				writer.WriteHeader(provider.tokenStatus)
				writeJSON(t, writer, map[string]any{"error": "invalid_grant"})
				return
			}
			if err := request.ParseForm(); err != nil {
				t.Errorf("解析 token 请求失败：%v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			provider.mutex.Lock()
			provider.lastVerifier = request.Form.Get("code_verifier")
			provider.mutex.Unlock()
			writeJSON(t, writer, map[string]any{
				"access_token": "test-access-token",
				"token_type":   "Bearer",
				"expires_in":   300,
				"id_token":     provider.signedIDToken(t),
			})
		case "/jwks":
			writeJSON(t, writer, map[string]any{"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "test-key",
					"use": "sig",
					"alg": "RS256",
					"n":   base64.RawURLEncoding.EncodeToString(provider.privateKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(provider.privateKey.E)).Bytes()),
				},
			}})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.server.Close)
	provider.endSessionEndpoint = provider.server.URL + "/logout"
	now := time.Now().UTC()
	provider.claims = map[string]any{
		"iss":   provider.server.URL,
		"sub":   "subject-1",
		"aud":   []string{"mail-suite-test"},
		"azp":   "mail-suite-test",
		"exp":   now.Add(5 * time.Minute).Unix(),
		"iat":   now.Unix(),
		"nonce": "test-nonce",
		"sid":   "session-1",
		"acr":   "urn:example:mfa",
		"amr":   []string{"pwd", "otp"},
	}
	return provider
}

// observedVerifier 并发安全地读取 token endpoint 最近收到的 verifier。
func (provider *testProvider) observedVerifier() string {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	return provider.lastVerifier
}

// signedIDToken 使用本地 RSA 测试密钥生成标准 RS256 ID token。
func (provider *testProvider) signedIDToken(t *testing.T) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	if err != nil {
		t.Fatalf("编码测试 JWT header 失败：%v", err)
	}
	payload, err := json.Marshal(provider.claims)
	if err != nil {
		t.Fatalf("编码测试 JWT claims 失败：%v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, provider.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名测试 ID token 失败：%v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// newTestClient 使用本地 TLS transport 完成真实 discovery。
func newTestClient(t *testing.T, provider *testProvider, timeout time.Duration) *Client {
	t.Helper()
	client, err := newWithHTTPClient(context.Background(), runtimeconfig.Config{
		Mode:                  runtimeconfig.ModeOIDC,
		Issuer:                provider.server.URL,
		ClientID:              "mail-suite-test",
		ClientSecret:          "test-client-secret",
		RedirectURI:           "https://mail.example.test/api/v1/auth/callback",
		PostLogoutRedirectURI: "https://mail.example.test/",
		Scopes:                []string{"openid", "profile"},
		SigningAlgorithms:     []string{"RS256"},
		ProviderTimeout:       timeout,
	}, provider.server.Client())
	if err != nil {
		t.Fatalf("创建测试 OIDC client 失败：%v", err)
	}
	return client
}

// writeJSON 写入测试 provider 的受控 JSON 响应。
func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("写入 OIDC 测试响应失败：%v", err)
	}
}
