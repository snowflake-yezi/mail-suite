package authorization

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

func TestNewContextValidatesMailboxInvariantAndSortsPermissions(t *testing.T) {
	view := &identity.SessionView{Session: identity.Session{Principal: identity.Principal{
		ID: uuid.New(), TenantID: uuid.New(), AccountType: identity.AccountTypeAdministrator,
	}, Permissions: []string{"z.permission", identity.PermissionPortalAdminAccess, "a.permission"}}, CSRFToken: "csrf-token"}
	context, err := NewContext(view, "request-1")
	if err != nil || !context.HasPermission(identity.PermissionPortalAdminAccess) {
		t.Fatalf("合法管理员上下文创建失败：context=%+v err=%v", context, err)
	}
	if got := context.Permissions(); got[0] != "a.permission" || got[2] != "z.permission" {
		t.Fatalf("权限应排序且返回副本：%v", got)
	}
	view.Session.Principal.Mailbox = &identity.Mailbox{ID: uuid.New()}
	if _, err := NewContext(view, "request-1"); !IsErrorCode(err, ErrorCodeAuthForbidden) {
		t.Fatalf("管理员绑定邮箱必须拒绝：%v", err)
	}
}

func TestRequireResourceSeparatesAdministratorAndMailbox(t *testing.T) {
	tenantID, mailboxID := uuid.New(), uuid.New()
	adminView := &identity.SessionView{Session: identity.Session{Principal: identity.Principal{
		ID: uuid.New(), TenantID: tenantID, AccountType: identity.AccountTypeAdministrator,
	}, Permissions: []string{identity.PermissionPortalAdminAccess}}, CSRFToken: "csrf"}
	admin, _ := NewContext(adminView, "request")
	if err := admin.RequireResource(Resource{TenantID: tenantID}); err != nil {
		t.Fatalf("管理员访问同租户资源失败：%v", err)
	}
	if !IsErrorCode(admin.RequireResource(Resource{TenantID: uuid.New()}), ErrorCodeNotFound) {
		t.Fatal("管理员不得探测跨租户资源")
	}
	mailboxView := &identity.SessionView{Session: identity.Session{Principal: identity.Principal{
		ID: uuid.New(), TenantID: tenantID, AccountType: identity.AccountTypeMailbox,
		Mailbox: &identity.Mailbox{ID: mailboxID},
	}, Permissions: []string{}}, CSRFToken: "csrf"}
	mailbox, _ := NewContext(mailboxView, "request")
	if err := mailbox.RequireResource(Resource{TenantID: tenantID, MailboxID: mailboxID}); err != nil {
		t.Fatalf("邮箱主体访问绑定资源失败：%v", err)
	}
	if !IsErrorCode(mailbox.RequireResource(Resource{TenantID: tenantID, MailboxID: uuid.New()}), ErrorCodeNotFound) {
		t.Fatal("邮箱主体不得访问其他 mailbox")
	}
}

func TestValidateMutationRequestChecksOriginCSRFContentAndIdempotency(t *testing.T) {
	view := &identity.SessionView{Session: identity.Session{Principal: identity.Principal{
		ID: uuid.New(), TenantID: uuid.New(), AccountType: identity.AccountTypeAdministrator,
	}, Permissions: []string{identity.PermissionPortalAdminAccess}}, CSRFToken: "csrf-token"}
	context, _ := NewContext(view, "request")
	request := httptest.NewRequest(http.MethodPost, "https://mail.example.test/api", nil)
	request.Header.Set("Origin", "https://mail.example.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "csrf-token")
	request.Header.Set("Idempotency-Key", "request-key-123456")
	if err := ValidateMutationRequest(request, "https://mail.example.test", context, true); err != nil {
		t.Fatalf("合法写请求被拒绝：%v", err)
	}
	request.Header.Del("Origin")
	request.Header.Set("Referer", "https://mail.example.test")
	if err := ValidateMutationRequest(request, "https://mail.example.test", context, true); err != nil {
		t.Fatalf("同源 Referer 回退应通过：%v", err)
	}
	request.Header.Set("Origin", "https://mail.example.test")
	request.Header.Set("Origin", "https://evil.example.test")
	if !IsErrorCode(ValidateMutationRequest(request, "https://mail.example.test", context, true), ErrorCodeCSRFInvalid) {
		t.Fatal("跨站请求必须拒绝")
	}
	request.Header.Set("Origin", "https://mail.example.test")
	request.Header.Set("X-CSRF-Token", "wrong")
	if !IsErrorCode(ValidateMutationRequest(request, "https://mail.example.test", context, true), ErrorCodeCSRFInvalid) {
		t.Fatal("错误 CSRF 必须拒绝")
	}
	request.Header.Set("X-CSRF-Token", "csrf-token")
	request.Header.Del("Idempotency-Key")
	if !IsErrorCode(ValidateMutationRequest(request, "https://mail.example.test", context, true), ErrorCodeInvalidArgument) {
		t.Fatal("缺少幂等键必须拒绝")
	}
}
