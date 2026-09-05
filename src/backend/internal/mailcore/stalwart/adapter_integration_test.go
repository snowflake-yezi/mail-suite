package stalwart

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

// TestAdapterAgainstRealStalwart 验证生产 adapter 与锁定 Stalwart 版本的真实管理协议契约。
func TestAdapterAgainstRealStalwart(t *testing.T) {
	if os.Getenv("MAIL_SUITE_STALWART_INTEGRATION") != "1" {
		t.Skip("未启用真实 Stalwart 集成测试")
	}

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("加载真实 Stalwart 配置失败：%v", err)
	}
	adapter, err := New(config)
	if err != nil {
		t.Fatalf("创建真实 Stalwart adapter 失败：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	logRealSessionContract(t, adapter, ctx)
	if err = adapter.Check(ctx); err != nil {
		t.Fatalf("真实 Stalwart adapter check 失败：%v", err)
	}

	mailboxID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	domainID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	operationID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	configurationHash := sha256.Sum256([]byte("stalwart-adapter-poc-revision-1"))
	deadline := time.Now().Add(60 * time.Second)
	query := mailcore.InspectMailboxQuery{
		MailboxID:         mailboxID,
		DomainID:          domainID,
		Address:           "adapter-poc@mail-suite.test",
		DesiredRevision:   1,
		ConfigurationHash: configurationHash,
		Deadline:          deadline,
	}

	observed, err := adapter.InspectMailbox(ctx, query)
	if err != nil {
		t.Fatalf("读取未创建邮箱失败：%v", err)
	}
	if observed.Status != mailcore.MailboxStatusAbsent || observed.MailboxID != mailboxID {
		t.Fatalf("未创建邮箱状态异常：status=%s", observed.Status)
	}

	command := mailcore.EnsureMailboxCommand{
		OperationID:       operationID,
		MailboxID:         mailboxID,
		DomainID:          domainID,
		Address:           query.Address,
		DesiredRevision:   1,
		ConfigurationHash: configurationHash,
		PayloadVersion:    mailcore.PayloadVersion,
		Deadline:          deadline,
	}
	if err = adapter.EnsureMailbox(ctx, command); err != nil {
		t.Fatalf("创建真实 Stalwart 邮箱失败：%v", err)
	}
	if err = adapter.EnsureMailbox(ctx, command); err != nil {
		t.Fatalf("重放真实 Stalwart 邮箱开通失败：%v", err)
	}
	assertRealObservation(t, adapter, ctx, query, 1, configurationHash)

	conflicting := command
	conflicting.ConfigurationHash = sha256.Sum256([]byte("conflicting-revision-1"))
	err = adapter.EnsureMailbox(ctx, conflicting)
	class, code := mailcore.ClassifyError(err)
	if class != mailcore.ErrorClassPermanent || code != ErrorCodeResourceConflict {
		t.Fatalf("同 revision 冲突分类错误：class=%s code=%s", class, code)
	}

	configurationHashV2 := sha256.Sum256([]byte("stalwart-adapter-poc-revision-2"))
	command.OperationID = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	command.DesiredRevision = 2
	command.ConfigurationHash = configurationHashV2
	command.Deadline = time.Now().Add(60 * time.Second)
	if err = adapter.EnsureMailbox(ctx, command); err != nil {
		t.Fatalf("推进真实 Stalwart 邮箱 revision 失败：%v", err)
	}
	query.DesiredRevision = 2
	query.ConfigurationHash = configurationHashV2
	query.Deadline = command.Deadline
	assertRealObservation(t, adapter, ctx, query, 2, configurationHashV2)
}

// logRealSessionContract 只记录 session 的公开结构，不输出认证信息或响应正文。
func logRealSessionContract(t *testing.T, adapter *Adapter, ctx context.Context) {
	t.Helper()
	requestURL := *adapter.endpoint
	requestURL.Path = "/jmap/session"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		t.Fatalf("构造真实 Stalwart session 请求失败：%v", err)
	}
	request.SetBasicAuth(adapter.adminUsername, adapter.adminPassword)
	response, err := adapter.httpClient.Do(request)
	if err != nil {
		t.Fatalf("请求真实 Stalwart session 失败：%v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var payload struct {
		APIURL       string                     `json:"apiUrl"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, maximumResponseBytes+1)).Decode(&payload); err != nil {
		t.Fatalf("解析真实 Stalwart session 公开结构失败：status=%d", response.StatusCode)
	}
	capabilities := make([]string, 0, len(payload.Capabilities))
	for capability := range payload.Capabilities {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	t.Logf("真实 Stalwart session：status=%d apiUrl=%q capabilities=%q", response.StatusCode, payload.APIURL, capabilities)
}

// assertRealObservation 核对真实 inspect 返回的 active、revision 与配置摘要。
func assertRealObservation(
	t *testing.T,
	adapter *Adapter,
	ctx context.Context,
	query mailcore.InspectMailboxQuery,
	wantRevision int64,
	wantHash [sha256.Size]byte,
) {
	t.Helper()
	observed, err := adapter.InspectMailbox(ctx, query)
	if err != nil {
		t.Fatalf("读取真实 Stalwart 邮箱失败：%v", err)
	}
	if observed.MailboxID != query.MailboxID || observed.Status != mailcore.MailboxStatusActive ||
		observed.Revision != wantRevision || observed.ConfigurationHash != wantHash ||
		observed.InspectedAt.IsZero() {
		t.Fatalf("真实 Stalwart 邮箱观测未收敛：status=%s revision=%d", observed.Status, observed.Revision)
	}
}
