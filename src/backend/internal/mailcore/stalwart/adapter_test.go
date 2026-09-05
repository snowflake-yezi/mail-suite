package stalwart

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

// fakeJMAPServer 实现 adapter contract 所需的最小 Stalwart JMAP 管理协议。
type fakeJMAPServer struct {
	mutex          sync.Mutex
	serverURL      string
	domainID       string
	domainName     string
	account        *accountObject
	setStatus      int
	setCalls       int
	authorizedOnly bool
}

// ServeHTTP 验证 Basic 管理身份并返回固定 JMAP session 或单方法响应。
func (server *fakeJMAPServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if username, password, ok := request.BasicAuth(); !ok || username != "admin" || password != "secret" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.authorizedOnly = true
	writer.Header().Set("Content-Type", "application/json")
	if request.URL.Path == "/jmap/session" {
		writeTestJSON(writer, map[string]any{
			"apiUrl": server.serverURL + "/jmap",
			"capabilities": map[string]any{
				"urn:ietf:params:jmap:core": map[string]any{},
				"urn:stalwart:jmap":         map[string]any{},
			},
		})
		return
	}
	if request.URL.Path != "/jmap" || request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	var envelope struct {
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
	var methodName string
	if json.Unmarshal(call[0], &methodName) != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if server.setStatus != 0 && methodName == "x:Account/set" {
		writer.WriteHeader(server.setStatus)
		return
	}
	result, ok := server.handleMethod(methodName, call[1])
	if !ok {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	writeTestJSON(writer, map[string]any{
		"methodResponses": []any{[]any{methodName, result, "c0"}},
	})
}

// handleMethod 执行唯一域和唯一账号的 query/get/set 测试状态机。
func (server *fakeJMAPServer) handleMethod(methodName string, rawArguments json.RawMessage) (any, bool) {
	switch methodName {
	case "x:Domain/query":
		return map[string]any{"ids": []string{server.domainID}}, true
	case "x:Domain/get":
		return map[string]any{"list": []any{domainObject{ID: server.domainID, Name: server.domainName}}}, true
	case "x:Account/query":
		if server.account == nil {
			return map[string]any{"ids": []string{}}, true
		}
		return map[string]any{"ids": []string{server.account.ID}}, true
	case "x:Account/get":
		if server.account == nil {
			return map[string]any{"list": []any{}}, true
		}
		return map[string]any{"list": []any{server.account}}, true
	case "x:Account/set":
		return server.handleSet(rawArguments)
	default:
		return nil, false
	}
}

// handleSet 保存 create/update 中 adapter 必须写入的非秘密账号字段。
func (server *fakeJMAPServer) handleSet(rawArguments json.RawMessage) (any, bool) {
	var arguments struct {
		Create map[string]json.RawMessage `json:"create"`
		Update map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(rawArguments, &arguments) != nil {
		return nil, false
	}
	server.setCalls++
	if raw, ok := arguments.Create["mail-suite"]; ok {
		var rawFields map[string]json.RawMessage
		var fields struct {
			Name        string `json:"name"`
			DomainID    string `json:"domainId"`
			Description string `json:"description"`
			Credentials map[string]struct {
				Secret string `json:"secret"`
			} `json:"credentials"`
		}
		if json.Unmarshal(raw, &rawFields) != nil || rawFields["isEnabled"] != nil ||
			json.Unmarshal(raw, &fields) != nil || fields.Credentials["0"].Secret == "" {
			return nil, false
		}
		server.account = &accountObject{
			ID:          "account-1",
			Name:        fields.Name,
			DomainID:    fields.DomainID,
			Description: fields.Description,
		}
		return map[string]any{"created": map[string]any{"mail-suite": map[string]any{"id": "account-1"}}}, true
	}
	for id, raw := range arguments.Update {
		if server.account == nil || id != server.account.ID {
			return nil, false
		}
		var rawFields map[string]json.RawMessage
		var fields struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(raw, &rawFields) != nil || rawFields["isEnabled"] != nil ||
			json.Unmarshal(raw, &fields) != nil {
			return nil, false
		}
		server.account.Description = fields.Description
		return map[string]any{"updated": map[string]any{id: nil}}, true
	}
	return nil, false
}

// TestAdapterCreatesInspectsAndReplaysMailbox 验证真实协议顺序、观测证据和同 revision 重放。
func TestAdapterCreatesInspectsAndReplaysMailbox(t *testing.T) {
	adapter, state, closeServer := newTestAdapter(t)
	defer closeServer()
	command := testEnsureCommand()

	if err := adapter.EnsureMailbox(context.Background(), command); err != nil {
		t.Fatalf("首次确保邮箱失败：%v", err)
	}
	observation, err := adapter.InspectMailbox(context.Background(), mailcore.InspectMailboxQuery{
		MailboxID:         command.MailboxID,
		DomainID:          command.DomainID,
		Address:           command.Address,
		DesiredRevision:   command.DesiredRevision,
		ConfigurationHash: command.ConfigurationHash,
		Deadline:          command.Deadline,
	})
	if err != nil {
		t.Fatalf("观测邮箱失败：%v", err)
	}
	if observation.Status != mailcore.MailboxStatusActive ||
		observation.Revision != command.DesiredRevision ||
		observation.ConfigurationHash != command.ConfigurationHash {
		t.Fatalf("观测结果未匹配目标：%+v", observation)
	}
	if err = adapter.EnsureMailbox(context.Background(), command); err != nil {
		t.Fatalf("重复确保邮箱失败：%v", err)
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if state.setCalls != 1 || !state.authorizedOnly {
		t.Fatalf("重复调用产生了额外副作用：setCalls=%d", state.setCalls)
	}
}

// TestAdapterRejectsMailboxOwnedByDifferentControlResource 验证地址冲突不会覆盖其他控制资源。
func TestAdapterRejectsMailboxOwnedByDifferentControlResource(t *testing.T) {
	adapter, state, closeServer := newTestAdapter(t)
	defer closeServer()
	command := testEnsureCommand()
	conflicting := command
	conflicting.MailboxID = uuid.New()
	state.account = &accountObject{
		ID:          "account-1",
		Name:        "receiver",
		DomainID:    state.domainID,
		Description: formatMetadata(conflicting),
	}

	err := adapter.EnsureMailbox(context.Background(), command)
	class, code := mailcore.ClassifyError(err)
	if class != mailcore.ErrorClassPermanent || code != ErrorCodeResourceConflict {
		t.Fatalf("地址冲突分类错误：class=%s code=%s err=%v", class, code, err)
	}
}

// TestAdapterClassifiesMutationTransportFailureAsUnknown 验证 set 失败不会被误判为确定未执行。
func TestAdapterClassifiesMutationTransportFailureAsUnknown(t *testing.T) {
	adapter, state, closeServer := newTestAdapter(t)
	defer closeServer()
	state.setStatus = http.StatusServiceUnavailable

	err := adapter.EnsureMailbox(context.Background(), testEnsureCommand())
	class, code := mailcore.ClassifyError(err)
	if class != mailcore.ErrorClassUnknown || code != ErrorCodeUnavailable {
		t.Fatalf("mutation 5xx 分类错误：class=%s code=%s err=%v", class, code, err)
	}
}

// TestAdapterRejectsCrossOriginSessionURL 验证服务端不能把管理凭据引向其他 origin。
func TestAdapterRejectsCrossOriginSessionURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(writer, map[string]any{
			"apiUrl": "https://other.invalid/jmap",
			"capabilities": map[string]any{
				"urn:ietf:params:jmap:core": map[string]any{},
				"urn:stalwart:jmap":         map[string]any{},
			},
		})
	}))
	defer server.Close()
	adapter := adapterForServer(t, server)

	err := adapter.Check(context.Background())
	class, code := mailcore.ClassifyError(err)
	if class != mailcore.ErrorClassPermanent || code != ErrorCodeProtocolInvalid {
		t.Fatalf("跨 origin session URL 分类错误：class=%s code=%s", class, code)
	}
}

// TestAdapterCheckRequiresWorkingStalwartManagementMethod 验证 readiness 会实际调用只读 registry 方法。
func TestAdapterCheckRequiresWorkingStalwartManagementMethod(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/jmap/session" {
			writeTestJSON(writer, map[string]any{
				"apiUrl":       server.URL + "/jmap",
				"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}},
			})
			return
		}
		writeTestJSON(writer, map[string]any{
			"methodResponses": []any{[]any{
				"error",
				map[string]any{"type": "unknownMethod"},
				"c0",
			}},
		})
	}))
	defer server.Close()
	adapter := adapterForServer(t, server)

	err := adapter.Check(context.Background())
	class, code := mailcore.ClassifyError(err)
	if class != mailcore.ErrorClassPermanent || code != ErrorCodeProtocolInvalid {
		t.Fatalf("不可用 Stalwart 管理方法的分类错误：class=%s code=%s", class, code)
	}
}

// TestMetadataRoundTrip 验证 description 中只保存固定控制标识和摘要。
func TestMetadataRoundTrip(t *testing.T) {
	command := testEnsureCommand()
	metadata, err := parseMetadata(formatMetadata(command))
	if err != nil {
		t.Fatalf("解析元数据失败：%v", err)
	}
	if metadata.MailboxID != command.MailboxID || metadata.DomainID != command.DomainID ||
		metadata.OperationID != command.OperationID || metadata.Revision != command.DesiredRevision ||
		metadata.ConfigurationHash != command.ConfigurationHash {
		t.Fatalf("元数据往返不一致：%+v", metadata)
	}
}

// newTestAdapter 创建带唯一 domain 和内存账号状态的 TLS JMAP 测试服务。
func newTestAdapter(t *testing.T) (*Adapter, *fakeJMAPServer, func()) {
	t.Helper()
	state := &fakeJMAPServer{domainID: "domain-1", domainName: "mail-suite.test"}
	server := httptest.NewTLSServer(state)
	state.serverURL = server.URL
	return adapterForServer(t, server), state, server.Close
}

// adapterForServer 使用测试服务证书构造启用真实 TLS 校验的 adapter。
func adapterForServer(t *testing.T, server *httptest.Server) *Adapter {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("解析测试 URL 失败：%v", err)
	}
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	adapter, err := New(Config{
		Endpoint:       endpoint,
		AdminUsername:  "admin",
		AdminPassword:  "secret",
		MailboxKey:     make([]byte, 32),
		RootCAs:        rootCAs,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("构造测试 adapter 失败：%v", err)
	}
	return adapter
}

// testEnsureCommand 返回字段完整且不会泄漏真实地址的测试开通命令。
func testEnsureCommand() mailcore.EnsureMailboxCommand {
	return mailcore.EnsureMailboxCommand{
		OperationID:       uuid.New(),
		MailboxID:         uuid.New(),
		DomainID:          uuid.New(),
		Address:           "receiver@mail-suite.test",
		DesiredRevision:   1,
		ConfigurationHash: sha256.Sum256([]byte("test-configuration")),
		PayloadVersion:    mailcore.PayloadVersion,
		Deadline:          time.Now().Add(time.Minute),
	}
}

// writeTestJSON 写入测试协议响应，不包含任何真实秘密。
func writeTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
