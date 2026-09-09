// Package oidcclient 使用标准 OIDC discovery、JWKS 和 OAuth2 PKCE 实现外部身份 adapter。
package oidcclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/runtimeconfig"
	"golang.org/x/oauth2"
)

const (
	maximumIssuedAtSkew     = 60 * time.Second
	maximumProviderResponse = int64(1 << 20)
)

var errProviderResponseTooLarge = errors.New("OIDC provider 响应超过大小上限")

// Client 封装 discovery 结果、OAuth2 code exchange 与 ID token 验证器。
type Client struct {
	oauthConfig       *oauth2.Config
	verifier          *oidc.IDTokenVerifier
	httpClient        *http.Client
	issuer            string
	clientID          string
	providerLogoutURL string
	now               func() time.Time
}

// discoveryMetadata 是启动时必须确认的最小 OIDC provider 能力集合。
type discoveryMetadata struct {
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	JWKSURI                       string   `json:"jwks_uri"`
	EndSessionEndpoint            string   `json:"end_session_endpoint"`
	ResponseTypesSupported        []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// tokenClaims 保存标准验证完成后仍需由产品策略检查的 ID token claim。
type tokenClaims struct {
	AuthorizedParty string   `json:"azp"`
	SessionID       string   `json:"sid"`
	ACR             string   `json:"acr"`
	AMR             []string `json:"amr"`
}

// boundedTransport 为 discovery、token 和 JWKS 响应统一施加读取上限。
type boundedTransport struct {
	base http.RoundTripper
	max  int64
}

// RoundTrip 包装响应体，超过上限时让协议解析明确失败。
func (transport *boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedReadCloser{body: response.Body, remaining: transport.max}
	return response, nil
}

// boundedReadCloser 在保留关闭语义的同时检测响应体是否超过允许字节数。
type boundedReadCloser struct {
	body      io.ReadCloser
	remaining int64
}

// Read 最多交付配置字节数，并通过稳定内部错误拒绝额外内容。
func (body *boundedReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	maximumRead := int64(len(buffer))
	if maximumRead > body.remaining+1 {
		maximumRead = body.remaining + 1
	}
	read, err := body.body.Read(buffer[:maximumRead])
	if int64(read) > body.remaining {
		allowed := int(body.remaining)
		body.remaining = 0
		return allowed, errProviderResponseTooLarge
	}
	body.remaining -= int64(read)
	return read, err
}

// Close 关闭底层 provider 响应体以释放连接。
func (body *boundedReadCloser) Close() error {
	return body.body.Close()
}

// New 创建使用有界 HTTP client 的 OIDC adapter，并在返回前完成 discovery 能力校验。
func New(ctx context.Context, config runtimeconfig.Config) (*Client, error) {
	return newWithHTTPClient(ctx, config, &http.Client{})
}

// newWithHTTPClient 允许测试注入只信任本地 TLS provider 的 transport。
func newWithHTTPClient(
	ctx context.Context,
	config runtimeconfig.Config,
	baseClient *http.Client,
) (*Client, error) {
	if config.Mode != runtimeconfig.ModeOIDC || baseClient == nil || config.ProviderTimeout <= 0 {
		return nil, errors.New("OIDC adapter 配置无效")
	}
	httpClient := *baseClient
	httpClient.Timeout = config.ProviderTimeout
	baseTransport := httpClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	httpClient.Transport = &boundedTransport{base: baseTransport, max: maximumProviderResponse}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	discoveryContext := oidc.ClientContext(ctx, &httpClient)
	provider, err := oidc.NewProvider(discoveryContext, config.Issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery 失败")
	}
	var metadata discoveryMetadata
	if err = provider.Claims(&metadata); err != nil {
		return nil, errors.New("OIDC discovery 响应无效")
	}
	if err = validateDiscovery(metadata); err != nil {
		return nil, err
	}
	providerLogoutURL, err := buildProviderLogoutURL(
		metadata.EndSessionEndpoint,
		config.ClientID,
		config.PostLogoutRedirectURI,
	)
	if err != nil {
		return nil, err
	}

	verifierContext := oidc.ClientContext(context.Background(), &httpClient)
	verifier := provider.VerifierContext(verifierContext, &oidc.Config{
		ClientID:             config.ClientID,
		SupportedSigningAlgs: append([]string(nil), config.SigningAlgorithms...),
	})
	return &Client{
		oauthConfig: &oauth2.Config{
			ClientID:     config.ClientID,
			ClientSecret: config.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  config.RedirectURI,
			Scopes:       append([]string(nil), config.Scopes...),
		},
		verifier:          verifier,
		httpClient:        &httpClient,
		issuer:            config.Issuer,
		clientID:          config.ClientID,
		providerLogoutURL: providerLogoutURL,
		now:               func() time.Time { return time.Now().UTC() },
	}, nil
}

// AuthorizationURL 构造带 nonce 和调用方预计算 S256 challenge 的授权跳转地址。
func (client *Client) AuthorizationURL(state, nonce, challenge string) (string, error) {
	if state == "" || nonce == "" || challenge == "" {
		return "", identity.NewError(identity.ErrorCodeInvalidRequest)
	}
	return client.oauthConfig.AuthCodeURL(
		state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	), nil
}

// ProviderLogoutURL 返回启动时验证并构造的前台退出地址，不包含用户凭据。
func (client *Client) ProviderLogoutURL() string {
	return client.providerLogoutURL
}

// Exchange 使用 authorization code 和 verifier 获取并校验 ID token 的全部认证证据。
func (client *Client) Exchange(ctx context.Context, code, verifier string) (identity.OIDCIdentity, error) {
	if code == "" {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if _, err := identity.PKCEChallenge(verifier); err != nil {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	requestContext := oidc.ClientContext(ctx, client.httpClient)
	oauthToken, err := client.oauthConfig.Exchange(requestContext, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity.OIDCIdentity{}, exchangeError(err)
	}
	rawIDToken, valid := oauthToken.Extra("id_token").(string)
	if !valid || rawIDToken == "" {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	idToken, err := client.verifier.Verify(requestContext, rawIDToken)
	if err != nil {
		return identity.OIDCIdentity{}, verificationError(err)
	}
	var claims tokenClaims
	if err = idToken.Claims(&claims); err != nil {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if idToken.Issuer != client.issuer || idToken.Subject == "" || idToken.Nonce == "" || idToken.IssuedAt.IsZero() {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if idToken.IssuedAt.After(client.now().Add(maximumIssuedAtSkew)) {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != client.clientID {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	if len(idToken.Audience) > 1 && claims.AuthorizedParty != client.clientID {
		return identity.OIDCIdentity{}, identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	return identity.OIDCIdentity{
		Issuer:    idToken.Issuer,
		Subject:   idToken.Subject,
		SessionID: claims.SessionID,
		Nonce:     idToken.Nonce,
		ACR:       claims.ACR,
		AMR:       append([]string(nil), claims.AMR...),
	}, nil
}

// validateDiscovery 拒绝降级端点和缺少 Authorization Code + PKCE S256 能力的 provider。
func validateDiscovery(metadata discoveryMetadata) error {
	for _, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint, metadata.JWKSURI} {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
			return errors.New("OIDC discovery 包含不安全端点")
		}
	}
	endSessionEndpoint, err := url.Parse(metadata.EndSessionEndpoint)
	if err != nil || endSessionEndpoint.Scheme != "https" || endSessionEndpoint.Host == "" ||
		endSessionEndpoint.User != nil || endSessionEndpoint.Fragment != "" || endSessionEndpoint.RawQuery != "" ||
		endSessionEndpoint.Opaque != "" {
		return errors.New("OIDC discovery 包含不安全的 end-session endpoint")
	}
	if !slices.Contains(metadata.ResponseTypesSupported, "code") ||
		!slices.Contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return errors.New("OIDC provider 不支持 Authorization Code + PKCE S256")
	}
	return nil
}

// buildProviderLogoutURL 只使用已验证配置构造不可变的浏览器前台退出地址。
func buildProviderLogoutURL(endpoint, clientID, postLogoutRedirectURI string) (string, error) {
	if clientID == "" || postLogoutRedirectURI == "" {
		return "", errors.New("OIDC 前台退出配置无效")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("OIDC 前台退出配置无效")
	}
	postLogoutRedirect, err := url.Parse(postLogoutRedirectURI)
	if err != nil || postLogoutRedirect.Scheme != "https" || postLogoutRedirect.Host == "" ||
		postLogoutRedirect.User != nil || postLogoutRedirect.Path != "/" || postLogoutRedirect.RawQuery != "" ||
		postLogoutRedirect.Fragment != "" || postLogoutRedirect.Opaque != "" {
		return "", errors.New("OIDC 前台退出配置无效")
	}
	query := parsed.Query()
	query.Set("client_id", clientID)
	query.Set("post_logout_redirect_uri", postLogoutRedirectURI)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// exchangeError 区分 provider 明确拒绝 code 与依赖网络不可用。
func exchangeError(err error) error {
	if isTransportError(err) {
		return identity.NewError(identity.ErrorCodePersistenceUnavailable)
	}
	var retrieveError *oauth2.RetrieveError
	if errors.As(err, &retrieveError) {
		return identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	return identity.NewError(identity.ErrorCodePersistenceUnavailable)
}

// verificationError 将 JWKS 网络故障映射为不可用，其余签名或 claim 失败视为无效流程。
func verificationError(err error) error {
	if isTransportError(err) {
		return identity.NewError(identity.ErrorCodePersistenceUnavailable)
	}
	return identity.NewError(identity.ErrorCodeAuthFlowInvalid)
}

// isTransportError 识别上下文截止和标准网络错误，不检查或回显错误文本。
func isTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	var urlError *url.Error
	return errors.As(err, &urlError)
}

var _ identity.OIDCProvider = (*Client)(nil)
