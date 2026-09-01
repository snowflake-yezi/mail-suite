// Package provisioning 定义邮箱开通意图的领域规则和应用服务边界。
package provisioning

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	localPartPattern   = regexp.MustCompile("^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+$")
	idempotencyPattern = regexp.MustCompile("^[A-Za-z0-9._:-]+$")
)

// ErrorCode 是可由 API adapter 稳定映射的业务错误标识。
type ErrorCode string

const (
	// ErrorCodeInvalidArgument 表示命令字段缺失或不满足业务约束。
	ErrorCodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	// ErrorCodeDomainUnavailable 隐藏域名不存在、停用和跨租户三种内部差异。
	ErrorCodeDomainUnavailable ErrorCode = "DOMAIN_UNAVAILABLE"
	// ErrorCodeMailboxAlreadyExists 表示域名内的规范化本地部分已被占用。
	ErrorCodeMailboxAlreadyExists ErrorCode = "MAILBOX_ALREADY_EXISTS"
	// ErrorCodeIdempotencyConflict 表示相同幂等键已绑定不同的规范化请求。
	ErrorCodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT"
	// ErrorCodeOperationNotFound 表示租户范围内没有对应 operation。
	ErrorCodeOperationNotFound ErrorCode = "OPERATION_NOT_FOUND"
	// ErrorCodePersistenceUnavailable 表示数据库无法完成或确认本次操作。
	ErrorCodePersistenceUnavailable ErrorCode = "PERSISTENCE_UNAVAILABLE"
)

// Error 保存稳定错误码和不包含底层实现细节的安全消息。
type Error struct {
	// Code 是调用方可用于分支处理的稳定业务错误码。
	Code ErrorCode
	// Message 是不包含 SQL、拓扑或凭据的中文错误说明。
	Message string
}

// Error 返回适合日志和上层协议映射的安全错误消息。
func (businessError *Error) Error() string {
	return businessError.Message
}

// NewError 根据稳定错误码创建不泄露基础设施细节的业务错误。
func NewError(code ErrorCode) *Error {
	messages := map[ErrorCode]string{
		ErrorCodeInvalidArgument:        "邮箱开通参数无效",
		ErrorCodeDomainUnavailable:      "邮件域名不可用于开通邮箱",
		ErrorCodeMailboxAlreadyExists:   "邮箱已经存在",
		ErrorCodeIdempotencyConflict:    "幂等键已用于不同请求",
		ErrorCodeOperationNotFound:      "operation 不存在",
		ErrorCodePersistenceUnavailable: "持久化服务暂不可用",
	}
	message, ok := messages[code]
	if !ok {
		code = ErrorCodePersistenceUnavailable
		message = messages[code]
	}
	return &Error{Code: code, Message: message}
}

// HasErrorCode 判断错误链中是否包含指定的稳定业务错误码。
func HasErrorCode(err error, code ErrorCode) bool {
	var businessError *Error
	return errors.As(err, &businessError) && businessError.Code == code
}

// RequestMailboxCommand 是上层身份授权完成后提交的原始邮箱开通命令。
type RequestMailboxCommand struct {
	// TenantID 是已授权 principal 所属租户，不得从未认证请求正文直接信任。
	TenantID uuid.UUID
	// DomainID 是已归属当前租户的邮件域名标识。
	DomainID uuid.UUID
	// LocalPart 是邮箱 @ 前的本地部分，v0.1 仅支持 ASCII 且大小写不敏感。
	LocalPart string
	// DisplayName 是可选显示名称，不参与邮箱唯一性判断。
	DisplayName string
	// IdempotencyKey 是调用方提供的租户内副作用去重键。
	IdempotencyKey string
}

// PreparedRequest 是通过领域校验并完成规范化的 repository 输入。
type PreparedRequest struct {
	// TenantID 是请求的租户隔离边界。
	TenantID uuid.UUID
	// DomainID 是目标邮件域名稳定标识。
	DomainID uuid.UUID
	// LocalPart 是小写规范化后的邮箱本地部分。
	LocalPart string
	// DisplayName 是去除首尾空白后的可选显示名称。
	DisplayName string
	// IdempotencyKey 是去除首尾空白后的租户内幂等键。
	IdempotencyKey string
	// RequestHash 是规范化业务参数的 SHA-256 指纹。
	RequestHash [sha256.Size]byte
}

// RequestMailboxResult 标识已持久化的邮箱意图及其异步 operation。
type RequestMailboxResult struct {
	// MailboxID 是邮箱期望状态的稳定标识。
	MailboxID uuid.UUID
	// OperationID 是后续状态查询和 worker 处理使用的稳定标识。
	OperationID uuid.UUID
	// Replayed 表示本次返回是否来自已存在的同指纹幂等请求。
	Replayed bool
}

// Operation 是租户范围内可查询的异步副作用状态。
type Operation struct {
	// ID 是 operation 稳定标识。
	ID uuid.UUID
	// TenantID 是 operation 所属租户标识。
	TenantID uuid.UUID
	// Kind 是副作用类型，v0.1 固定为 mailbox.provision。
	Kind string
	// ResourceType 是目标资源类型，v0.1 固定为 mailbox。
	ResourceType string
	// ResourceID 是目标邮箱稳定标识。
	ResourceID uuid.UUID
	// Status 是 operation 当前持久化状态。
	Status string
}

// Repository 定义领域服务所需的原子持久化和租户隔离查询能力。
type Repository interface {
	RequestMailbox(context.Context, PreparedRequest) (RequestMailboxResult, error)
	GetOperation(context.Context, uuid.UUID, uuid.UUID) (Operation, error)
}

// Service 执行邮箱开通输入校验、规范化和持久化编排。
type Service struct {
	repository Repository
}

// NewService 创建只依赖领域 repository 契约的邮箱开通服务。
func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// RequestMailbox 校验并规范化开通命令，再交由 repository 原子持久化。
func (service *Service) RequestMailbox(
	ctx context.Context,
	command RequestMailboxCommand,
) (RequestMailboxResult, error) {
	prepared, err := prepareRequest(command)
	if err != nil {
		return RequestMailboxResult{}, err
	}
	return service.repository.RequestMailbox(ctx, prepared)
}

// GetOperation 按租户边界查询 operation，拒绝空标识进入持久化层。
func (service *Service) GetOperation(
	ctx context.Context,
	tenantID uuid.UUID,
	operationID uuid.UUID,
) (Operation, error) {
	if tenantID == uuid.Nil || operationID == uuid.Nil {
		return Operation{}, NewError(ErrorCodeInvalidArgument)
	}
	return service.repository.GetOperation(ctx, tenantID, operationID)
}

// prepareRequest 固定所有影响唯一性和请求指纹的规范化规则。
func prepareRequest(command RequestMailboxCommand) (PreparedRequest, error) {
	if command.TenantID == uuid.Nil || command.DomainID == uuid.Nil {
		return PreparedRequest{}, NewError(ErrorCodeInvalidArgument)
	}

	localPart := strings.ToLower(command.LocalPart)
	if len(localPart) == 0 || len(localPart) > 64 ||
		localPart != strings.TrimSpace(localPart) ||
		strings.HasPrefix(localPart, ".") || strings.HasSuffix(localPart, ".") ||
		strings.Contains(localPart, "..") || !localPartPattern.MatchString(localPart) {
		return PreparedRequest{}, NewError(ErrorCodeInvalidArgument)
	}

	displayName := strings.TrimSpace(command.DisplayName)
	if !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > 128 ||
		strings.IndexFunc(displayName, unicode.IsControl) >= 0 {
		return PreparedRequest{}, NewError(ErrorCodeInvalidArgument)
	}

	idempotencyKey := strings.TrimSpace(command.IdempotencyKey)
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 ||
		!idempotencyPattern.MatchString(idempotencyKey) {
		return PreparedRequest{}, NewError(ErrorCodeInvalidArgument)
	}

	fingerprintInput := struct {
		DomainID    string `json:"domain_id"`
		LocalPart   string `json:"local_part"`
		DisplayName string `json:"display_name"`
	}{
		DomainID:    command.DomainID.String(),
		LocalPart:   localPart,
		DisplayName: displayName,
	}
	encodedFingerprint, _ := json.Marshal(fingerprintInput)

	return PreparedRequest{
		TenantID:       command.TenantID,
		DomainID:       command.DomainID,
		LocalPart:      localPart,
		DisplayName:    displayName,
		IdempotencyKey: idempotencyKey,
		RequestHash:    sha256.Sum256(encodedFingerprint),
	}, nil
}
