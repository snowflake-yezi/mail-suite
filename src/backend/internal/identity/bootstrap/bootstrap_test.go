package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authorizationhttp "github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization/httpapi"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/httpapi"
)

// staticSessionResolver 为装配回归提供有效的服务端会话。
type staticSessionResolver struct {
	view *identity.SessionView
}

// ResolveSession 返回固定测试会话，不连接外部数据库。
func (resolver *staticSessionResolver) ResolveSession(context.Context, string) (*identity.SessionView, error) {
	return resolver.view, nil
}

func TestAPIRoutesRegistersAuthenticationOnlyAndKeepsAuthorizationBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	view := &identity.SessionView{Session: identity.Session{
		Principal:   identity.Principal{ID: uuid.New(), TenantID: uuid.New(), AccountType: identity.AccountTypeAdministrator},
		Permissions: []string{identity.PermissionPortalAdminAccess},
	}, CSRFToken: "csrf-token"}
	boundary, err := authorizationhttp.New(&staticSessionResolver{view: view}, "https://mail.example.test")
	if err != nil {
		t.Fatalf("创建共享授权边界失败：%v", err)
	}
	routes := &apiRoutes{authentication: &httpapi.Handler{}, authorization: boundary}
	router := gin.New()
	router.Use(func(ctx *gin.Context) { ctx.Set("request_id", "request-test") })
	routes.RegisterRoutes(router)
	registered := router.Routes()
	if len(registered) != 4 {
		t.Fatalf("当前只能注册四条认证路由，实际为 %d", len(registered))
	}
	for _, route := range registered {
		if route.Path != "/api/v1/auth/login" && route.Path != "/api/v1/auth/callback" &&
			route.Path != "/api/v1/session" && route.Path != "/api/v1/auth/logout" {
			t.Fatalf("意外暴露业务路由：%s", route.Path)
		}
	}
	router.GET("/test-protected", routes.authorization.Protect(identity.AccountTypeAdministrator, false), func(ctx *gin.Context) {
		ctx.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/test-protected", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("共享授权边界无法供后续业务路由使用：%d", response.Code)
	}
}

func TestRoutesDisabledModeRegistersNoAuthenticationEndpoints(t *testing.T) {
	t.Setenv("MAIL_SUITE_AUTH_MODE", "disabled")
	registrar, err := Routes(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("disabled 模式不应构造 OIDC 或数据库 adapter：%v", err)
	}
	if registrar != nil {
		t.Fatal("disabled 模式不得注册认证路由")
	}
}

func TestRoutesRejectsMissingExplicitMode(t *testing.T) {
	t.Setenv("MAIL_SUITE_AUTH_MODE", "")
	if _, err := Routes(context.Background(), nil, nil); err == nil {
		t.Fatal("API 缺少显式认证模式必须启动失败")
	}
}
