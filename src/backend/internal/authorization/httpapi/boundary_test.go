package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

// fakeResolver 记录受保护路由实际使用的会话查询，不保存 Cookie 原值。
type fakeResolver struct {
	view  *identity.SessionView
	err   error
	calls int
}

// ResolveSession 为每次请求提供测试会话或预设错误。
func (resolver *fakeResolver) ResolveSession(_ context.Context, _ string) (*identity.SessionView, error) {
	resolver.calls++
	return resolver.view, resolver.err
}

// testResponse 保存公共错误包，便于逐字段核对状态与脱敏边界。
type testResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func TestProtectRejectsAnonymousWrongAccountAndMissingPermission(t *testing.T) {
	admin := testView(identity.AccountTypeAdministrator)
	mailbox := testView(identity.AccountTypeMailbox)
	noPermission := testView(identity.AccountTypeAdministrator)
	noPermission.Session.Permissions = nil
	tests := []struct {
		name string
		view *identity.SessionView
		code int
		id   string
	}{
		{"anonymous", nil, http.StatusUnauthorized, string(authorization.ErrorCodeAuthRequired)},
		{"mailbox on admin route", mailbox, http.StatusForbidden, string(authorization.ErrorCodeAuthForbidden)},
		{"admin without permission", noPermission, http.StatusForbidden, string(authorization.ErrorCodeAuthForbidden)},
		{"valid admin", admin, http.StatusOK, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{view: test.view}
			calls := 0
			router := testBoundaryRouter(t, resolver, identity.AccountTypeAdministrator, false, func(ctx *gin.Context) {
				calls++
				authorized, exists := Context(ctx)
				if !exists || authorized.RequestID() != "request-test" || authorized.CSRFVerified() {
					t.Fatal("handler 应取得已验证的请求授权上下文")
				}
				ctx.Status(http.StatusOK)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
			if response.Code != test.code || calls != boolInt(test.code == http.StatusOK) || resolver.calls != 1 {
				t.Fatalf("授权入口错误：status=%d handler=%d resolve=%d", response.Code, calls, resolver.calls)
			}
			if test.id != "" {
				assertError(t, response, test.id)
			}
		})
	}
}

func TestProtectMailboxUsesBoundContextAndRejectsUnexpectedBody(t *testing.T) {
	resolver := &fakeResolver{view: testView(identity.AccountTypeMailbox)}
	mailboxID := resolver.view.Session.Principal.Mailbox.ID
	router := testBoundaryRouter(t, resolver, identity.AccountTypeMailbox, false, func(ctx *gin.Context) {
		authorized, ok := Context(ctx)
		if !ok || authorized.MailboxID() != mailboxID || authorized.RequireMailbox(uuid.New()) == nil {
			t.Fatal("mailbox handler 必须使用会话绑定的 mailbox 标识")
		}
		ctx.Status(http.StatusOK)
	})
	valid := httptest.NewRecorder()
	router.ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/protected", nil))
	if valid.Code != http.StatusOK {
		t.Fatalf("绑定 mailbox 读取被拒绝：%d", valid.Code)
	}
	withBody := httptest.NewRecorder()
	router.ServeHTTP(withBody, httptest.NewRequest(http.MethodGet, "/protected", strings.NewReader("unexpected")))
	assertError(t, withBody, string(authorization.ErrorCodeInvalidArgument))
	unknownBody := httptest.NewRequest(http.MethodGet, "/protected", nil)
	unknownBody.Body = io.NopCloser(strings.NewReader("hidden"))
	unknownBody.ContentLength = 0
	unknownResponse := httptest.NewRecorder()
	router.ServeHTTP(unknownResponse, unknownBody)
	assertError(t, unknownResponse, string(authorization.ErrorCodeInvalidArgument))
}

func TestProtectRejectsInvalidMutationsBeforeHandler(t *testing.T) {
	tests := []struct {
		name   string
		change func(*http.Request)
		code   authorization.ErrorCode
	}{
		{"wrong csrf", func(req *http.Request) { req.Header.Set("X-CSRF-Token", "wrong") }, authorization.ErrorCodeCSRFInvalid},
		{"cross origin", func(req *http.Request) { req.Header.Set("Origin", "https://evil.example.test") }, authorization.ErrorCodeCSRFInvalid},
		{"duplicate origin", func(req *http.Request) { req.Header.Add("Origin", "https://mail.example.test") }, authorization.ErrorCodeCSRFInvalid},
		{"duplicate csrf", func(req *http.Request) { req.Header.Add("X-CSRF-Token", "csrf-token") }, authorization.ErrorCodeCSRFInvalid},
		{"no origin", func(req *http.Request) { req.Header.Del("Origin") }, authorization.ErrorCodeCSRFInvalid},
		{"wrong content type", func(req *http.Request) { req.Header.Set("Content-Type", "text/plain") }, authorization.ErrorCodeInvalidArgument},
		{"duplicate content type", func(req *http.Request) { req.Header.Add("Content-Type", "application/json") }, authorization.ErrorCodeInvalidArgument},
		{"no idempotency", func(req *http.Request) { req.Header.Del("Idempotency-Key") }, authorization.ErrorCodeInvalidArgument},
		{"duplicate idempotency", func(req *http.Request) { req.Header.Add("Idempotency-Key", "request-key-123456") }, authorization.ErrorCodeInvalidArgument},
		{"spaced idempotency", func(req *http.Request) { req.Header.Set("Idempotency-Key", " request-key-123456 ") }, authorization.ErrorCodeInvalidArgument},
		{"large known body", func(req *http.Request) { req.ContentLength = authorization.DefaultMaxJSONBodyBytes + 1 }, authorization.ErrorCodeInvalidArgument},
		{"large chunked body", func(req *http.Request) {
			req.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", int(authorization.DefaultMaxJSONBodyBytes)+1)))
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
		}, authorization.ErrorCodeInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			router := testBoundaryRouter(t, &fakeResolver{view: testView(identity.AccountTypeAdministrator)}, identity.AccountTypeAdministrator, true, func(ctx *gin.Context) {
				calls++
				ctx.Status(http.StatusOK)
			})
			request := validMutation()
			test.change(request)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assertError(t, response, string(test.code))
			if calls != 0 {
				t.Fatal("无效请求不得进入有副作用的 handler")
			}
		})
	}
}

func TestProtectRestoresBoundBodyAndAcceptsSameOriginReferer(t *testing.T) {
	resolver := &fakeResolver{view: testView(identity.AccountTypeAdministrator)}
	router := testBoundaryRouter(t, resolver, identity.AccountTypeAdministrator, true, func(ctx *gin.Context) {
		authorized, ok := Context(ctx)
		if !ok || !authorized.CSRFVerified() {
			t.Fatal("合法写请求进入 handler 前必须完成 CSRF 校验")
		}
		body, err := io.ReadAll(ctx.Request.Body)
		if err != nil || string(body) != `{"name":"ok"}` {
			t.Fatalf("有界请求体未交给业务 handler：body=%q err=%v", body, err)
		}
		ctx.Status(http.StatusNoContent)
	})
	request := validMutation()
	request.Header.Del("Origin")
	request.Header.Set("Referer", "https://mail.example.test/mail/inbox")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("合法同源写请求失败：%d %s", response.Code, response.Body.String())
	}
}

func TestWriteErrorMapsKnownCodesAndRedactsUnknowns(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   authorization.ErrorCode
	}{
		{identity.NewError(identity.ErrorCodeAuthRequired), http.StatusUnauthorized, authorization.ErrorCodeAuthRequired},
		{identity.NewError(identity.ErrorCodeAuthForbidden), http.StatusForbidden, authorization.ErrorCodeAuthForbidden},
		{authorization.NewError(authorization.ErrorCodeOperationNotFound), http.StatusNotFound, authorization.ErrorCodeOperationNotFound},
		{authorization.NewError(authorization.ErrorCodeIdempotencyConflict), http.StatusConflict, authorization.ErrorCodeIdempotencyConflict},
		{authorization.NewError(authorization.ErrorCodeUpstreamTimeout), http.StatusGatewayTimeout, authorization.ErrorCodeUpstreamTimeout},
		{authorization.NewError(authorization.ErrorCodeUpstreamUnavailable), http.StatusServiceUnavailable, authorization.ErrorCodeUpstreamUnavailable},
		{&authorization.Error{Code: "UNSAFE", Message: "DSN=secret"}, http.StatusServiceUnavailable, authorization.ErrorCodePersistenceUnavailable},
		{errors.New("DSN=secret"), http.StatusServiceUnavailable, authorization.ErrorCodePersistenceUnavailable},
	}
	for _, test := range tests {
		router := gin.New()
		router.Use(func(ctx *gin.Context) { ctx.Set("request_id", "request-test") })
		router.GET("/protected", func(ctx *gin.Context) { WriteError(ctx, test.err) })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
		assertError(t, response, string(test.code))
		if response.Code != test.status || strings.Contains(response.Body.String(), "DSN=secret") {
			t.Fatalf("错误状态或脱敏失败：status=%d body=%q", response.Code, response.Body.String())
		}
	}
	resolver := &fakeResolver{err: errors.New("DSN=secret")}
	router := testBoundaryRouter(t, resolver, identity.AccountTypeAdministrator, false, func(ctx *gin.Context) {
		t.Fatal("会话解析失败不得进入业务 handler")
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	assertError(t, response, string(authorization.ErrorCodePersistenceUnavailable))
}

func testView(accountType identity.AccountType) *identity.SessionView {
	principal := identity.Principal{ID: uuid.New(), TenantID: uuid.New(), AccountType: accountType}
	if accountType == identity.AccountTypeMailbox {
		principal.Mailbox = &identity.Mailbox{ID: uuid.New()}
	}
	return &identity.SessionView{Session: identity.Session{
		Principal: principal, Permissions: []string{identity.PermissionPortalAdminAccess},
	}, CSRFToken: "csrf-token"}
}

func testBoundaryRouter(t *testing.T, resolver *fakeResolver, accountType identity.AccountType, idempotency bool, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(ctx *gin.Context) { ctx.Set("request_id", "request-test") })
	boundary, err := New(resolver, "https://mail.example.test")
	if err != nil {
		t.Fatalf("创建授权入口失败：%v", err)
	}
	router.GET("/protected", boundary.Protect(accountType, idempotency), handler)
	router.POST("/protected", boundary.Protect(accountType, idempotency), handler)
	return router
}

func validMutation() *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/protected", bytes.NewBufferString(`{"name":"ok"}`))
	request.Header.Set("Origin", "https://mail.example.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "csrf-token")
	request.Header.Set("Idempotency-Key", "request-key-123456")
	return request
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var result testResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error.Code != code || result.Error.RequestID != "request-test" || result.Error.Message == "" {
		t.Fatalf("公共错误包无效：status=%d body=%q err=%v", response.Code, response.Body.String(), err)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("授权错误不得被缓存")
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
