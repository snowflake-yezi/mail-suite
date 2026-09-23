package postgres

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization"
	authorizationhttp "github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization/httpapi"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/migrations"
)

// authorizationTestProvider 满足身份服务构造约束，本测试不触发 OIDC 登录流程。
type authorizationTestProvider struct{}

// AuthorizationURL 不参与服务端会话读取测试。
func (authorizationTestProvider) AuthorizationURL(string, string, string) (string, error) {
	return "", nil
}

// Exchange 不参与服务端会话读取测试。
func (authorizationTestProvider) Exchange(context.Context, string, string) (identity.OIDCIdentity, error) {
	return identity.OIDCIdentity{}, nil
}

// ProviderLogoutURL 提供身份服务构造所需的受信任测试地址。
func (authorizationTestProvider) ProviderLogoutURL() string {
	return "https://idp.example.test/logout"
}

func TestAuthorizationBoundaryUsesCurrentPostgresSession(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过授权边界真实 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	if err := migrations.Run(ctx, databaseURL, migrations.CommandUp, io.Discard); err != nil {
		t.Fatalf("应用授权集成测试 migration 失败：%v", err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("创建授权集成测试连接池失败：%v", err)
	}
	t.Cleanup(pool.Close)
	fixtures := insertIdentityFixtures(t, ctx, pool)
	t.Cleanup(func() { deleteIdentityFixtures(t, ctx, pool, fixtures) })
	repository, err := NewIdentityRepository(pool, identity.DefaultSessionIdleTimeout, identity.DefaultSessionTouchInterval)
	if err != nil {
		t.Fatalf("创建身份 repository 失败：%v", err)
	}
	hasher, err := identity.NewSecretHasher([]byte("authorization-test-pepper-32-bytes!!"))
	if err != nil {
		t.Fatalf("创建测试摘要器失败：%v", err)
	}
	pkce, err := identity.NewPKCECipher("test-v1", map[string][]byte{"test-v1": []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatalf("创建测试 PKCE 密钥失败：%v", err)
	}
	service, err := identity.NewService(repository, authorizationTestProvider{}, hasher, pkce, identity.MFAPolicy{RequiredAMR: []string{"otp"}})
	if err != nil {
		t.Fatalf("创建身份服务失败：%v", err)
	}
	adminToken := "admin-session-" + fixtures.suffix
	mailboxToken := "mailbox-session-" + fixtures.suffix
	createAuthorizationSession(t, ctx, repository, hasher, fixtures, fixtures.adminPrincipalID, fixtures.adminSubject, adminToken)
	createAuthorizationSession(t, ctx, repository, hasher, fixtures, fixtures.mailboxPrincipalID, fixtures.mailboxSubject, mailboxToken)

	boundary, err := authorizationhttp.New(service, "https://mail.example.test")
	if err != nil {
		t.Fatalf("创建授权边界失败：%v", err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(request *gin.Context) { request.Set("request_id", "authorization-integration") })
	router.GET("/admin/:tenant", boundary.Protect(identity.AccountTypeAdministrator, false), func(request *gin.Context) {
		authorized, ok := authorizationhttp.Context(request)
		if !ok {
			t.Fatal("管理请求缺少授权上下文")
		}
		tenantID, _ := uuid.Parse(request.Param("tenant"))
		if err := authorized.RequireResource(authorization.Resource{TenantID: tenantID}); err != nil {
			authorizationhttp.WriteError(request, err)
			return
		}
		request.Status(http.StatusNoContent)
	})
	router.GET("/mailbox/:mailbox", boundary.Protect(identity.AccountTypeMailbox, false), func(request *gin.Context) {
		authorized, ok := authorizationhttp.Context(request)
		if !ok {
			t.Fatal("邮箱请求缺少授权上下文")
		}
		mailboxID, _ := uuid.Parse(request.Param("mailbox"))
		if err := authorized.RequireResource(authorization.Resource{TenantID: fixtures.tenantID, MailboxID: mailboxID}); err != nil {
			authorizationhttp.WriteError(request, err)
			return
		}
		request.Status(http.StatusNoContent)
	})

	cases := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"admin own tenant", "/admin/" + fixtures.tenantID.String(), adminToken, http.StatusNoContent},
		{"admin other tenant", "/admin/" + fixtures.otherTenantID.String(), adminToken, http.StatusNotFound},
		{"mailbox own resource", "/mailbox/" + fixtures.mailboxID.String(), mailboxToken, http.StatusNoContent},
		{"mailbox other resource", "/mailbox/" + uuid.NewString(), mailboxToken, http.StatusNotFound},
		{"mailbox on admin route", "/admin/" + fixtures.tenantID.String(), mailboxToken, http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := authorizationRequest(router, test.path, test.token); got != test.status {
				t.Fatalf("实际 HTTP 状态为 %d，期望 %d", got, test.status)
			}
		})
	}
	if err := repository.RevokeSession(ctx, hasher.Digest(identity.SecretPurposeSessionCookie, adminToken), time.Now().UTC(), identity.RevocationReasonLogout); err != nil {
		t.Fatalf("撤销测试管理会话失败：%v", err)
	}
	if got := authorizationRequest(router, "/admin/"+fixtures.tenantID.String(), adminToken); got != http.StatusUnauthorized {
		t.Fatalf("已撤销会话仍可访问业务资源：status=%d", got)
	}
}

func createAuthorizationSession(t *testing.T, ctx context.Context, repository *IdentityRepository, hasher *identity.SecretHasher, fixtures identityTestFixtures, principalID uuid.UUID, subject, token string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := repository.CreateSession(ctx, identity.SessionToCreate{
		ID:                uuid.New(),
		SessionDigest:     hasher.Digest(identity.SecretPurposeSessionCookie, token),
		PrincipalID:       principalID,
		OIDCIssuer:        fixtures.issuer,
		OIDCSubject:       subject,
		CSRFDigest:        hasher.Digest(identity.SecretPurposeCSRFToken, hasher.DeriveCSRFToken(token)),
		CreatedAt:         now,
		IdleExpiresAt:     now.Add(identity.DefaultSessionIdleTimeout),
		AbsoluteExpiresAt: now.Add(identity.DefaultSessionAbsoluteTimeout),
	})
	if err != nil {
		t.Fatalf("创建授权集成测试会话失败：%v", err)
	}
}

func authorizationRequest(router *gin.Engine, path, token string) int {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code
}
