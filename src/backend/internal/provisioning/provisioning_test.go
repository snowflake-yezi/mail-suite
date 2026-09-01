package provisioning

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// repositoryStub 捕获领域服务传入的规范化请求，不执行外部 I/O。
type repositoryStub struct {
	prepared     PreparedRequest
	requestCalls int
	result       RequestMailboxResult
	operation    Operation
}

// RequestMailbox 保存规范化请求并返回预设结果。
func (repository *repositoryStub) RequestMailbox(
	_ context.Context,
	request PreparedRequest,
) (RequestMailboxResult, error) {
	repository.prepared = request
	repository.requestCalls++
	return repository.result, nil
}

// GetOperation 返回预设 operation，供服务边界测试使用。
func (repository *repositoryStub) GetOperation(
	_ context.Context,
	_ uuid.UUID,
	_ uuid.UUID,
) (Operation, error) {
	return repository.operation, nil
}

func TestRequestMailboxNormalizesFieldsBeforePersistence(t *testing.T) {
	tenantID := uuid.New()
	domainID := uuid.New()
	expected := RequestMailboxResult{MailboxID: uuid.New(), OperationID: uuid.New()}
	repository := &repositoryStub{result: expected}
	service := NewService(repository)

	result, err := service.RequestMailbox(context.Background(), RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "Alice.Sales",
		DisplayName:    "  Alice  ",
		IdempotencyKey: "  request:alice-0001  ",
	})
	if err != nil {
		t.Fatalf("合法开通请求失败：%v", err)
	}
	if result != expected {
		t.Fatalf("服务返回结果不一致：got=%+v want=%+v", result, expected)
	}
	if repository.prepared.LocalPart != "alice.sales" || repository.prepared.DisplayName != "Alice" {
		t.Fatalf("字段未按契约规范化：%+v", repository.prepared)
	}
	if repository.prepared.IdempotencyKey != "request:alice-0001" {
		t.Fatalf("幂等键未按契约规范化：%q", repository.prepared.IdempotencyKey)
	}
	if repository.prepared.RequestHash == [32]byte{} {
		t.Fatal("规范化请求必须生成非空 SHA-256 指纹")
	}
}

func TestRequestMailboxUsesSameFingerprintForEquivalentInput(t *testing.T) {
	tenantID := uuid.New()
	domainID := uuid.New()
	repository := &repositoryStub{}
	service := NewService(repository)

	_, err := service.RequestMailbox(context.Background(), RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "ALICE",
		DisplayName:    " Alice ",
		IdempotencyKey: "request-alice-0001",
	})
	if err != nil {
		t.Fatalf("首次等价请求失败：%v", err)
	}
	firstHash := repository.prepared.RequestHash

	_, err = service.RequestMailbox(context.Background(), RequestMailboxCommand{
		TenantID:       tenantID,
		DomainID:       domainID,
		LocalPart:      "alice",
		DisplayName:    "Alice",
		IdempotencyKey: "request-alice-0001",
	})
	if err != nil {
		t.Fatalf("第二次等价请求失败：%v", err)
	}
	if repository.prepared.RequestHash != firstHash {
		t.Fatal("等价规范化请求应生成相同指纹")
	}
}

func TestRequestMailboxRejectsInvalidFields(t *testing.T) {
	validTenantID := uuid.New()
	validDomainID := uuid.New()
	valid := RequestMailboxCommand{
		TenantID:       validTenantID,
		DomainID:       validDomainID,
		LocalPart:      "alice",
		DisplayName:    "Alice",
		IdempotencyKey: "request-alice-0001",
	}
	testCases := []struct {
		name   string
		mutate func(*RequestMailboxCommand)
	}{
		{name: "missing tenant", mutate: func(command *RequestMailboxCommand) { command.TenantID = uuid.Nil }},
		{name: "missing domain", mutate: func(command *RequestMailboxCommand) { command.DomainID = uuid.Nil }},
		{name: "empty local part", mutate: func(command *RequestMailboxCommand) { command.LocalPart = "" }},
		{name: "long local part", mutate: func(command *RequestMailboxCommand) { command.LocalPart = strings.Repeat("a", 65) }},
		{name: "leading dot", mutate: func(command *RequestMailboxCommand) { command.LocalPart = ".alice" }},
		{name: "trailing dot", mutate: func(command *RequestMailboxCommand) { command.LocalPart = "alice." }},
		{name: "consecutive dots", mutate: func(command *RequestMailboxCommand) { command.LocalPart = "alice..sales" }},
		{name: "unicode local part", mutate: func(command *RequestMailboxCommand) { command.LocalPart = "用户" }},
		{name: "local whitespace", mutate: func(command *RequestMailboxCommand) { command.LocalPart = " alice" }},
		{name: "short idempotency key", mutate: func(command *RequestMailboxCommand) { command.IdempotencyKey = "too-short" }},
		{name: "invalid idempotency key", mutate: func(command *RequestMailboxCommand) { command.IdempotencyKey = "request/alice/0001" }},
		{name: "long display name", mutate: func(command *RequestMailboxCommand) { command.DisplayName = strings.Repeat("名", 129) }},
		{name: "control in display name", mutate: func(command *RequestMailboxCommand) { command.DisplayName = "Alice\nAdmin" }},
		{name: "invalid UTF-8 display name", mutate: func(command *RequestMailboxCommand) { command.DisplayName = string([]byte{0xff}) }},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			command := valid
			testCase.mutate(&command)
			repository := &repositoryStub{}
			_, err := NewService(repository).RequestMailbox(context.Background(), command)
			if !HasErrorCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("无效字段应返回 INVALID_ARGUMENT，实际为 %v", err)
			}
			if repository.requestCalls != 0 {
				t.Fatal("无效请求不得进入持久化层")
			}
		})
	}
}

func TestGetOperationRejectsEmptyIdentifiers(t *testing.T) {
	repository := &repositoryStub{}
	_, err := NewService(repository).GetOperation(context.Background(), uuid.Nil, uuid.New())
	if !HasErrorCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("空租户标识应返回 INVALID_ARGUMENT，实际为 %v", err)
	}
}
