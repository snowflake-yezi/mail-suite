package stalwart

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

// readJMAPServer 模拟管理映射和邮箱读取两个严格隔离的 JMAP 身份。
type readJMAPServer struct {
	mutex            sync.Mutex
	serverURL        string
	mailboxPassword  string
	account          accountObject
	emails           []emailObject
	queryState       string
	mailStatus       int
	mailDelay        time.Duration
	mailCalls        int
	mailCapabilities bool
	accountMissing   bool
	mailSessionURL   string
}

// ServeHTTP 按请求身份和单方法协议返回固定版 JMAP 测试响应。
func (server *readJMAPServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	username, password, ok := request.BasicAuth()
	admin := ok && username == "admin" && password == "secret"
	mailbox := ok && username == "receiver@mail-suite.test" && password == server.mailboxPassword
	if !admin && !mailbox {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	if request.URL.Path == "/jmap/session" && request.Method == http.MethodGet {
		capabilities := map[string]any{coreCapability: map[string]any{}}
		primary := map[string]string{}
		if mailbox && server.mailCapabilities {
			capabilities[mailCapability] = map[string]any{}
			primary[mailCapability] = "mail-account-1"
		}
		apiURL := server.serverURL + "/jmap"
		if mailbox && server.mailSessionURL != "" {
			apiURL = server.mailSessionURL
		}
		writeTestJSON(writer, map[string]any{
			"apiUrl": apiURL, "capabilities": capabilities,
			"primaryAccounts": primary,
		})
		return
	}
	if request.URL.Path != "/jmap" || request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	var envelope struct {
		Using       []string          `json:"using"`
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if json.NewDecoder(request.Body).Decode(&envelope) != nil || len(envelope.MethodCalls) != 1 {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var call []json.RawMessage
	if json.Unmarshal(envelope.MethodCalls[0], &call) != nil || len(call) != 3 {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var method string
	if json.Unmarshal(call[0], &method) != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(method, "Email/") {
		if !mailbox || len(envelope.Using) != 2 || envelope.Using[1] != mailCapability {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		server.mailCalls++
		if server.mailDelay > 0 {
			time.Sleep(server.mailDelay)
		}
		if server.mailStatus != 0 {
			writer.WriteHeader(server.mailStatus)
			return
		}
	} else if !admin {
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	result, valid := server.handle(method, call[1])
	if !valid {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	writeTestJSON(writer, map[string]any{"methodResponses": []any{[]any{method, result, "c0"}}})
}

// handle 执行精确域/账号映射和有界 Email 查询测试状态机。
func (server *readJMAPServer) handle(method string, raw json.RawMessage) (any, bool) {
	switch method {
	case "x:Domain/query":
		return map[string]any{"ids": []string{"domain-1"}}, true
	case "x:Domain/get":
		return map[string]any{"list": []domainObject{{ID: "domain-1", Name: "mail-suite.test"}}}, true
	case "x:Account/query":
		if server.accountMissing {
			return map[string]any{"ids": []string{}}, true
		}
		return map[string]any{"ids": []string{"account-1"}}, true
	case "x:Account/get":
		return map[string]any{"list": []accountObject{server.account}}, true
	case "Email/query":
		var args struct {
			AccountID string `json:"accountId"`
			Position  int    `json:"position"`
			Limit     int    `json:"limit"`
		}
		if json.Unmarshal(raw, &args) != nil || args.AccountID != "mail-account-1" ||
			args.Position < 0 || args.Limit < 1 || args.Limit > maximumPageSize+1 {
			return nil, false
		}
		ids := make([]string, 0)
		for index := args.Position; index < len(server.emails) && len(ids) < args.Limit; index++ {
			ids = append(ids, server.emails[index].ID)
		}
		return map[string]any{"ids": ids, "queryState": server.queryState, "position": args.Position}, true
	case "Email/get":
		var args struct {
			AccountID           string   `json:"accountId"`
			IDs                 []string `json:"ids"`
			FetchHTMLBodyValues bool     `json:"fetchHTMLBodyValues"`
			MaxBodyValueBytes   int      `json:"maxBodyValueBytes"`
		}
		if json.Unmarshal(raw, &args) != nil || args.AccountID != "mail-account-1" ||
			args.FetchHTMLBodyValues || (args.MaxBodyValueBytes != 0 && args.MaxBodyValueBytes != maximumTextBytes) {
			return nil, false
		}
		list := make([]emailObject, 0, len(args.IDs))
		notFound := make([]string, 0)
		for _, id := range args.IDs {
			found := false
			for _, email := range server.emails {
				if email.ID == id {
					list = append(list, email)
					found = true
					break
				}
			}
			if !found {
				notFound = append(notFound, id)
			}
		}
		return map[string]any{"list": list, "notFound": notFound}, true
	default:
		return nil, false
	}
}

// newReadTestAdapter 创建同源 TLS 测试服务和准确的控制面映射。
func newReadTestAdapter(t *testing.T) (*Adapter, *readJMAPServer, mailcore.ReadScope, func()) {
	t.Helper()
	command := testEnsureCommand()
	state := &readJMAPServer{
		account: accountObject{
			ID: "account-1", Name: "receiver", DomainID: "domain-1",
			Description: formatMetadata(command),
		},
		queryState: "state-1", mailCapabilities: true,
	}
	server := httptest.NewTLSServer(state)
	state.serverURL = server.URL
	adapter := adapterForServer(t, server)
	state.mailboxPassword = adapter.mailboxPassword(command.MailboxID, command.DesiredRevision)
	scope := mailcore.ReadScope{
		MailboxID: command.MailboxID, DomainID: command.DomainID,
		Address: command.Address, Revision: command.DesiredRevision,
		ConfigurationHash: command.ConfigurationHash, Deadline: time.Now().Add(time.Minute),
	}
	return adapter, state, scope, server.Close
}

// fixtureEmail 返回包含纯文本、HTML 标记和附件的结构化邮件。
func fixtureEmail(id string, age int) emailObject {
	return emailObject{
		ID: id, ReceivedAt: time.Now().Add(-time.Duration(age) * time.Minute).UTC().Format(time.RFC3339),
		From:    []mailcore.MailAddress{{Name: "Sender", Email: "sender@example.test"}},
		To:      []mailcore.MailAddress{{Email: "receiver@mail-suite.test"}},
		Subject: "Test subject", Preview: "Preview", Size: 124,
		HasAttachment: true,
		TextBody:      []emailBodyPart{{PartID: "text-1"}},
		HTMLBody:      []emailBodyPart{{PartID: "html-1"}},
		BodyValues: map[string]bodyValue{"text-1": {Value: "Hello, world"},
			"html-1": {Value: "<script>unsafe()</script>"}},
		Attachments: []emailBodyPart{{BlobID: "private-blob-1", Name: "report.txt", Type: "text/plain", Size: 12}},
	}
}

// TestReadAdapterUsesMailboxIdentityAndPaginates 验证隔离身份、两页和详情字段。
func TestReadAdapterUsesMailboxIdentityAndPaginates(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	state.emails = []emailObject{fixtureEmail("private-message-1", 1), fixtureEmail("private-message-2", 2), fixtureEmail("private-message-3", 3)}
	first, err := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 2})
	if err != nil || len(first.Messages) != 2 || first.NextCursor == "" {
		t.Fatalf("第一页读取失败：messages=%d cursor=%t err=%v", len(first.Messages), first.NextCursor != "", err)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(first.Messages[0].ID)
	if strings.Contains(string(decoded), "private-message") || strings.Contains(first.NextCursor, "private-message") {
		t.Fatal("公开令牌泄漏原始 JMAP ID")
	}
	detail, err := adapter.GetMessage(context.Background(), scope, first.Messages[0].ID)
	if err != nil || detail.Text != "Hello, world" || !detail.HasHTML || len(detail.Attachments) != 1 ||
		detail.Summary.ID != first.Messages[0].ID {
		t.Fatalf("详情内容不符：detail=%+v err=%v", detail, err)
	}
	if strings.Contains(detail.Text, "<script>") || strings.Contains(detail.Attachments[0].Reference, "private-blob") {
		t.Fatal("详情泄漏 HTML 或原始 Blob ID")
	}
	second, err := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Messages) != 1 || second.NextCursor != "" {
		t.Fatalf("续页读取失败：messages=%d cursor=%t err=%v", len(second.Messages), second.NextCursor != "", err)
	}
	state.mutex.Lock()
	mailCalls := state.mailCalls
	state.mutex.Unlock()
	if mailCalls != 5 {
		t.Fatalf("预期两次 query、三次 get，实际 %d 次", mailCalls)
	}
}

// TestReadAdapterRejectsMappingAndInvalidTokens 验证映射漂移、跨邮箱、篡改与过期。
func TestReadAdapterRejectsMappingAndInvalidTokens(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	state.emails = []emailObject{fixtureEmail("private-message-1", 1), fixtureEmail("private-message-2", 2)}
	page, err := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	other := scope
	other.ConfigurationHash = sha256.Sum256([]byte("other mailbox configuration"))
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: other, Limit: 1})
		return readErr
	}(), ErrorCodeResourceConflict)
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: other, Limit: 1, Cursor: page.NextCursor})
		return readErr
	}(), ErrorCodeCursorInvalid)
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.GetMessage(context.Background(), other, page.Messages[0].ID)
		return readErr
	}(), ErrorCodeReferenceInvalid)
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.GetMessage(context.Background(), scope, page.Messages[0].ID+"x")
		return readErr
	}(), ErrorCodeReferenceInvalid)
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.GetMessage(context.Background(), scope, page.NextCursor)
		return readErr
	}(), ErrorCodeReferenceInvalid)
	state.mutex.Lock()
	state.queryState = "state-2"
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1, Cursor: page.NextCursor})
		return readErr
	}(), ErrorCodeCursorInvalid)
	adapter.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.GetMessage(context.Background(), scope, page.Messages[0].ID)
		return readErr
	}(), ErrorCodeReferenceInvalid)
}

// TestReadAdapterRejectsMissingMappingAndForeignSession 验证缺失映射、版本漂移和外部 URL。
func TestReadAdapterRejectsMissingMappingAndForeignSession(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	state.mutex.Lock()
	state.accountMissing = true
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeMailboxNotFound)
	state.mutex.Lock()
	state.accountMissing = false
	state.mutex.Unlock()
	newRevision := scope
	newRevision.Revision++
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: newRevision, Limit: 1})
		return readErr
	}(), ErrorCodeResourceConflict)
	state.mutex.Lock()
	state.mailSessionURL = "https://foreign.example.test/jmap"
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeProtocolInvalid)
}

// TestReadAdapterBoundsTextAndAttachmentReferences 验证正文上限及附件令牌绑定。
func TestReadAdapterBoundsTextAndAttachmentReferences(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	email := fixtureEmail("private-message-1", 1)
	email.BodyValues["text-1"] = bodyValue{Value: strings.Repeat("a", maximumTextBytes+100), IsTruncated: true}
	state.mutex.Lock()
	state.emails = []emailObject{email}
	state.mutex.Unlock()
	page, err := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := adapter.GetMessage(context.Background(), scope, page.Messages[0].ID)
	if err != nil || len(detail.Text) != maximumTextBytes || !detail.TextTruncated {
		t.Fatalf("正文大小或截断标记错误：size=%d truncated=%t err=%v", len(detail.Text), detail.TextTruncated, err)
	}
	reference, err := adapter.openReadToken(detail.Attachments[0].Reference, scope, "attachment")
	if err != nil || reference.MessageID != email.ID || reference.BlobID != email.Attachments[0].BlobID {
		t.Fatalf("附件引用与当前邮件绑定错误：%v", err)
	}
	other := scope
	other.ConfigurationHash = sha256.Sum256([]byte("other configuration"))
	assertReadErrorCode(t, func() error {
		_, tokenErr := adapter.openReadToken(detail.Attachments[0].Reference, other, "attachment")
		return tokenErr
	}(), ErrorCodeReferenceInvalid)
}

// TestReadAdapterHandlesMissingMailAndUpstreamFailure 验证空箱、删除和上游失败。
func TestReadAdapterHandlesMissingMailAndUpstreamFailure(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	page, err := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 5})
	if err != nil || len(page.Messages) != 0 || page.NextCursor != "" {
		t.Fatalf("空箱结果异常：%+v err=%v", page, err)
	}
	state.emails = []emailObject{fixtureEmail("private-message-1", 1)}
	page, err = adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("准备邮件引用失败：%v", err)
	}
	state.mutex.Lock()
	state.emails = nil
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.GetMessage(context.Background(), scope, page.Messages[0].ID)
		return readErr
	}(), ErrorCodeMessageNotFound)
	state.mutex.Lock()
	state.mailStatus = http.StatusServiceUnavailable
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeUnavailable)
	state.mutex.Lock()
	state.mailStatus = 0
	state.mailCapabilities = false
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeProtocolInvalid)
}

// TestReadAdapterSeparatesTimeoutFromUnavailable 验证网关和 deadline 超时分类。
func TestReadAdapterSeparatesTimeoutFromUnavailable(t *testing.T) {
	adapter, state, scope, closeServer := newReadTestAdapter(t)
	defer closeServer()
	state.mutex.Lock()
	state.mailStatus = http.StatusGatewayTimeout
	state.mutex.Unlock()
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeTimeout)
	state.mutex.Lock()
	state.mailStatus = 0
	state.mailDelay = 40 * time.Millisecond
	state.mutex.Unlock()
	scope.Deadline = time.Now().Add(10 * time.Millisecond)
	assertReadErrorCode(t, func() error {
		_, readErr := adapter.ListMessages(context.Background(), mailcore.ListMessagesQuery{Scope: scope, Limit: 1})
		return readErr
	}(), ErrorCodeTimeout)
}

// assertReadErrorCode 验证对外只保留稳定脱敏错误码。
func assertReadErrorCode(t *testing.T, err error, expected string) {
	t.Helper()
	_, actual := mailcore.ClassifyError(err)
	if actual != expected {
		t.Fatalf("错误码不符：expected=%s actual=%s err=%v", expected, actual, err)
	}
}
