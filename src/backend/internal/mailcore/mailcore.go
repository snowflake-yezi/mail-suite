// Package mailcore 定义控制面调用邮件内核的业务能力边界。
package mailcore

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// PayloadVersion 是当前邮箱开通命令的稳定载荷版本。
const PayloadVersion uint32 = 1

// MailboxStatus 表示 adapter 从邮件内核实际读取到的邮箱状态。
type MailboxStatus string

const (
	// MailboxStatusAbsent 表示邮件内核中不存在目标邮箱。
	MailboxStatusAbsent MailboxStatus = "absent"
	// MailboxStatusActive 表示邮件内核接受目标邮箱作为活跃收件人。
	MailboxStatusActive MailboxStatus = "active"
	// MailboxStatusSuspended 表示邮件内核保留邮箱但不允许其正常服务。
	MailboxStatusSuspended MailboxStatus = "suspended"
)

// ErrorClass 描述 worker 可据以决定重试方式的安全错误分类。
type ErrorClass string

const (
	// ErrorClassRetryable 表示已确认没有完成副作用且可以退避重试。
	ErrorClassRetryable ErrorClass = "retryable"
	// ErrorClassPermanent 表示请求确定性失败，原样重试不会成功。
	ErrorClassPermanent ErrorClass = "permanent"
	// ErrorClassUnknown 表示副作用可能已经完成，必须先 inspect 再决定后续动作。
	ErrorClassUnknown ErrorClass = "unknown"
)

const (
	// ErrorCodeUnknown 是未归一化错误的稳定安全错误码。
	ErrorCodeUnknown = "MAIL_CORE_OUTCOME_UNKNOWN"
	// ErrorCodeNotConverged 表示 apply 后 inspect 尚未观察到目标状态。
	ErrorCodeNotConverged = "MAIL_CORE_NOT_CONVERGED"
	// ErrorCodeNewerRevision 表示邮件内核已经观察到比当前 operation 更新的资源版本。
	ErrorCodeNewerRevision = "MAIL_CORE_NEWER_REVISION"
)

var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// Error 保存 adapter 返回给 worker 的稳定分类和脱敏错误码。
type Error struct {
	// Class 决定 worker 是退避重试、确定失败还是优先 inspect。
	Class ErrorClass
	// Code 是不包含地址、endpoint、凭据或底层响应正文的稳定错误码。
	Code string
}

// Error 返回可安全记录的稳定错误码。
func (adapterError *Error) Error() string {
	return adapterError.Code
}

// NewError 创建经过约束的 adapter 错误；无效分类或错误码会归一化为 unknown。
func NewError(class ErrorClass, code string) *Error {
	if !validErrorClass(class) || !errorCodePattern.MatchString(code) {
		return &Error{Class: ErrorClassUnknown, Code: ErrorCodeUnknown}
	}
	return &Error{Class: class, Code: code}
}

// ClassifyError 将任意 adapter 错误归一化为 worker 可持久化的安全分类和错误码。
func ClassifyError(err error) (ErrorClass, string) {
	if err == nil {
		return "", ""
	}
	var adapterError *Error
	if errors.As(err, &adapterError) && validErrorClass(adapterError.Class) &&
		errorCodePattern.MatchString(adapterError.Code) {
		return adapterError.Class, adapterError.Code
	}
	return ErrorClassUnknown, ErrorCodeUnknown
}

// EnsureMailboxCommand 描述一次不可变邮箱开通 operation 的邮件内核命令。
type EnsureMailboxCommand struct {
	// OperationID 是本次逻辑状态变更的稳定幂等身份。
	OperationID uuid.UUID
	// MailboxID 是控制面邮箱稳定标识，不得由邮箱地址替代。
	MailboxID uuid.UUID
	// DomainID 是邮箱所属邮件域名的控制面稳定标识。
	DomainID uuid.UUID
	// Address 是邮件内核需要开通的规范化完整地址，不得写入普通日志。
	Address string
	// DesiredRevision 是本次 operation 负责收敛的不可变期望版本。
	DesiredRevision int64
	// ConfigurationHash 是期望邮箱配置的 SHA-256 摘要，不包含可逆业务数据。
	ConfigurationHash [32]byte
	// PayloadVersion 固定 adapter 对命令字段和语义的解释版本。
	PayloadVersion uint32
	// Deadline 是本次外部调用必须停止的绝对时间，并早于 lease 失效时间。
	Deadline time.Time
}

// InspectMailboxQuery 描述不会产生副作用的邮箱实际状态查询。
type InspectMailboxQuery struct {
	// MailboxID 是需要从邮件内核核对的控制面邮箱标识。
	MailboxID uuid.UUID
	// DomainID 是邮箱所属邮件域名的控制面稳定标识。
	DomainID uuid.UUID
	// Address 是需要核对的规范化完整地址，不得写入普通日志。
	Address string
	// DesiredRevision 指明本次核对所对应的期望版本，但不得用于短路实际读取。
	DesiredRevision int64
	// ConfigurationHash 是需要与实际配置比较的期望 SHA-256 摘要。
	ConfigurationHash [32]byte
	// Deadline 是本次只读查询必须停止的绝对时间。
	Deadline time.Time
}

// ObservedMailbox 表示 adapter 通过实际读取获得的邮箱状态证据。
type ObservedMailbox struct {
	// MailboxID 是本次观测对应的控制面邮箱标识。
	MailboxID uuid.UUID
	// Status 是邮件内核当前实际邮箱状态。
	Status MailboxStatus
	// Revision 是 adapter 从实际状态证据确定的资源版本。
	Revision int64
	// ConfigurationHash 是实际邮箱配置的稳定 SHA-256 摘要。
	ConfigurationHash [32]byte
	// InspectedAt 是 adapter 完成实际读取的时间。
	InspectedAt time.Time
}

// Adapter 按业务能力隔离具体邮件内核协议和内部数据模型。
type Adapter interface {
	// EnsureMailbox 幂等确保目标 operation revision 的邮箱存在并处于期望状态。
	EnsureMailbox(context.Context, EnsureMailboxCommand) error
	// InspectMailbox 始终读取邮件内核实际状态，不得复用历史 ensure 成功结果。
	InspectMailbox(context.Context, InspectMailboxQuery) (ObservedMailbox, error)
}

// validErrorClass 判断错误分类是否属于稳定契约集合。
func validErrorClass(class ErrorClass) bool {
	switch class {
	case ErrorClassRetryable, ErrorClassPermanent, ErrorClassUnknown:
		return true
	default:
		return false
	}
}
