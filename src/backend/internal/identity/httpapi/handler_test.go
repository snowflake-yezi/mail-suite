package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

// fakeAuthService 让 HTTP 测试精确控制领域结果并观察敏感值传递边界。
type fakeAuthService struct {
	loginStart     identity.LoginStart
	loginErr       error
	callbackResult identity.CallbackResult
	callbackErr    error
	rejectErr      error
	sessionView    *identity.SessionView
	sessionErr     error
	logoutErr      error
	callbackInput  identity.CallbackInput
	logoutToken    string
	logoutCSRF     string
	logoutCalls    int
}

// StartLogin 返回预设的授权跳转。
func (service *fakeAuthService) StartLogin(
	context.Context,
	string,
	identity.RequestMetadata,
) (identity.LoginStart, error) {
	return service.loginStart, service.loginErr
}

// CompleteLogin 记录 callback 输入并返回预设会话。
func (service *fakeAuthService) CompleteLogin(
	_ context.Context,
	input identity.CallbackInput,
) (identity.CallbackResult, error) {
	service.callbackInput = input
	return service.callbackResult, service.callbackErr
}

// RejectLogin 返回预设 provider 拒绝处理结果。
func (service *fakeAuthService) RejectLogin(context.Context, string, string, identity.RequestMetadata) error {
	if service.rejectErr == nil {
		return identity.NewError(identity.ErrorCodeAuthFlowInvalid)
	}
	return service.rejectErr
}

// ResolveSession 返回预设会话视图。
func (service *fakeAuthService) ResolveSession(context.Context, string) (*identity.SessionView, error) {
	return service.sessionView, service.sessionErr
}

// Logout 记录 Cookie 与 CSRF header 后返回预设结果。
func (service *fakeAuthService) Logout(
	_ context.Context,
	sessionToken string,
	csrfToken string,
	_ identity.RequestMetadata,
) error {
	service.logoutCalls++
	service.logoutToken = sessionToken
	service.logoutCSRF = csrfToken
	return service.logoutErr
}

func TestLoginSetsHostCookieAndRedirectsToProvider(t *testing.T) {
	expiresAt := time.Now().UTC().Add(identity.DefaultAuthFlowLifetime)
	service := &fakeAuthService{loginStart: identity.LoginStart{
		AuthorizationURL: "https://idp.example.test/authorize?state=opaque",
		BrowserCookie:    "browser-cookie",
		ExpiresAt:        expiresAt,
	}}
	router := testRouter(t, service)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login?return_to=%2Fmail%2Finbox", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != service.loginStart.AuthorizationURL {
		t.Fatalf("登录发起响应错误：code=%d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
	cookie := findResponseCookie(t, recorder, identity.AuthFlowCookieName)
	assertSecureHostCookie(t, cookie)
	if cookie.Value != "browser-cookie" || cookie.MaxAge != int(identity.DefaultAuthFlowLifetime.Seconds()) {
		t.Fatalf("登录流程 Cookie 值或生命周期错误：%+v", cookie)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("登录响应必须禁止缓存")
	}
}

func TestCallbackClearsFlowCookieAndRotatesSessionCookie(t *testing.T) {
	service := &fakeAuthService{callbackResult: identity.CallbackResult{
		SessionToken: "new-session-token",
		ExpiresAt:    time.Now().UTC().Add(identity.DefaultSessionAbsoluteTimeout),
		RedirectTo:   "/mail/inbox",
	}}
	router := testRouter(t, service)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?state=state&code=code", nil)
	request.AddCookie(&http.Cookie{Name: identity.AuthFlowCookieName, Value: "browser-cookie"})
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/mail/inbox" {
		t.Fatalf("callback 成功跳转错误：code=%d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
	if service.callbackInput.BrowserCookie != "browser-cookie" || service.callbackInput.Metadata.RequestID != "request-test" {
		t.Fatalf("callback 未传递浏览器关联值或 request ID：%+v", service.callbackInput)
	}
	flowCookie := findResponseCookie(t, recorder, identity.AuthFlowCookieName)
	if flowCookie.MaxAge >= 0 {
		t.Fatalf("callback 必须清理临时 Cookie：%+v", flowCookie)
	}
	sessionCookie := findResponseCookie(t, recorder, identity.SessionCookieName)
	assertSecureHostCookie(t, sessionCookie)
	if sessionCookie.Value != "new-session-token" {
		t.Fatalf("callback 未轮换新会话 Cookie：%+v", sessionCookie)
	}
}

func TestCallbackRejectsAmbiguousCodeAndError(t *testing.T) {
	service := &fakeAuthService{}
	router := testRouter(t, service)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback?state=state&code=code&error=denied", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "AUTH_FLOW_INVALID") {
		t.Fatalf("互斥 callback 参数应返回通用流程错误：code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if service.callbackInput.Code != "" {
		t.Fatal("参数互斥失败时不得进入领域 callback")
	}
}

func TestSessionReturnsAnonymousOrAuthenticatedDiscriminatedResponse(t *testing.T) {
	service := &fakeAuthService{}
	router := testRouter(t, service)
	anonymousRecorder := httptest.NewRecorder()
	router.ServeHTTP(anonymousRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if anonymousRecorder.Code != http.StatusOK || anonymousRecorder.Body.String() != "{\"authenticated\":false}" {
		t.Fatalf("匿名会话响应不稳定：%s", anonymousRecorder.Body.String())
	}

	service.sessionView = &identity.SessionView{
		Session: identity.Session{
			ID: uuid.New(),
			Principal: identity.Principal{
				ID: uuid.New(), TenantID: uuid.New(), AccountType: identity.AccountTypeMailbox,
				DisplayName: "Alice", Mailbox: &identity.Mailbox{ID: uuid.New(), Address: "alice@example.test"},
			},
			Permissions:       []string{},
			IdleExpiresAt:     time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC),
			AbsoluteExpiresAt: time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC),
		},
		CSRFToken: "csrf-token-value-with-at-least-32-bytes",
	}
	authenticatedRecorder := httptest.NewRecorder()
	authenticatedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	authenticatedRequest.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "session"})
	router.ServeHTTP(authenticatedRecorder, authenticatedRequest)
	body := authenticatedRecorder.Body.String()
	if authenticatedRecorder.Code != http.StatusOK || !strings.Contains(body, `"account_type":"mailbox"`) ||
		!strings.Contains(body, `"mailbox":{"id":`) || !strings.Contains(body, `"permissions":[]`) {
		t.Fatalf("已认证会话响应不符合判别联合：%s", body)
	}
}

func TestLogoutRequiresTrustedOriginAndClearsCookieAfterSuccess(t *testing.T) {
	service := &fakeAuthService{}
	router := testRouter(t, service)
	crossOriginRecorder := httptest.NewRecorder()
	crossOriginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	crossOriginRequest.Header.Set("Origin", "https://evil.example.test")
	router.ServeHTTP(crossOriginRecorder, crossOriginRequest)
	if crossOriginRecorder.Code != http.StatusForbidden || service.logoutCalls != 0 {
		t.Fatalf("跨站退出必须在领域调用前拒绝：code=%d calls=%d", crossOriginRecorder.Code, service.logoutCalls)
	}

	successRecorder := httptest.NewRecorder()
	successRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	successRequest.Header.Set("Origin", "https://mail.example.test")
	successRequest.Header.Set("X-CSRF-Token", "csrf-token")
	successRequest.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "session-token"})
	router.ServeHTTP(successRecorder, successRequest)
	if successRecorder.Code != http.StatusNoContent || service.logoutCalls != 1 ||
		service.logoutToken != "session-token" || service.logoutCSRF != "csrf-token" {
		t.Fatalf("同源退出调用错误：code=%d calls=%d", successRecorder.Code, service.logoutCalls)
	}
	cleared := findResponseCookie(t, successRecorder, identity.SessionCookieName)
	if cleared.MaxAge >= 0 {
		t.Fatalf("退出成功后必须清理会话 Cookie：%+v", cleared)
	}
}

func TestLogoutRejectsUnexpectedRequestBody(t *testing.T) {
	service := &fakeAuthService{}
	router := testRouter(t, service)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", strings.NewReader("unexpected"))
	request.Header.Set("Origin", "https://mail.example.test")
	request.Header.Set("Content-Type", "text/plain")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || service.logoutCalls != 0 {
		t.Fatalf("带请求体的退出必须在领域调用前拒绝：code=%d calls=%d", recorder.Code, service.logoutCalls)
	}
}

// testRouter 创建带受控 request ID 的 Gin 测试路由。
func testRouter(t *testing.T, service authService) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(ctx *gin.Context) {
		ctx.Set("request_id", "request-test")
		ctx.Next()
	})
	handler, err := New(service, "https://mail.example.test")
	if err != nil {
		t.Fatalf("创建认证 HTTP handler 失败：%v", err)
	}
	handler.RegisterRoutes(router)
	return router
}

// findResponseCookie 按名称查找 recorder 中的 Set-Cookie。
func findResponseCookie(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("响应缺少 Cookie %s：%v", name, recorder.Header().Values("Set-Cookie"))
	return nil
}

// assertSecureHostCookie 验证 __Host- Cookie 的固定安全属性。
func assertSecureHostCookie(t *testing.T, cookie *http.Cookie) {
	t.Helper()
	if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("Cookie 安全属性错误：%+v", cookie)
	}
}
