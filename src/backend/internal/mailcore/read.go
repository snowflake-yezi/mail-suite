package mailcore

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ReadScope 是上层已经授权的单个控制面邮箱及其期望配置。
type ReadScope struct {
	// MailboxID 是控制面的邮箱授权主键。
	MailboxID uuid.UUID
	// DomainID 是邮箱所属控制面域名标识。
	DomainID uuid.UUID
	// Address 是规范化邮箱地址，仅用于内部映射和认证。
	Address string
	// Revision 是本次读取要求的准确配置版本。
	Revision int64
	// ConfigurationHash 是本次读取要求的配置摘要。
	ConfigurationHash [32]byte
	// Deadline 是包括映射检查和邮件查询在内的绝对截止时间。
	Deadline time.Time
}

// MailAddress 表示经过 JMAP 结构化读取的邮件地址。
type MailAddress struct {
	// Name 是发件人或收件人的显示名称。
	Name string
	// Email 是该地址的邮箱字符串。
	Email string
}

// MessageSummary 是不包含 mail-core 内部标识的列表元数据。
type MessageSummary struct {
	// ID 是绑定控制面邮箱的短时不透明邮件引用。
	ID string
	// ReceivedAt 是 Stalwart 记录的收件时间。
	ReceivedAt time.Time
	// From 是邮件声称的发件人地址。
	From []MailAddress
	// Subject 是邮件主题，可为空。
	Subject string
	// Preview 是 Stalwart 生成的纯文本预览。
	Preview string
	// Size 是邮件的字节大小。
	Size int64
	// HasAttachment 表示 JMAP 报告存在附件。
	HasAttachment bool
}

// ListMessagesQuery 请求一个已授权邮箱中固定排序的有界邮件页。
type ListMessagesQuery struct {
	// Scope 是需重新核对的控制面邮箱映射。
	Scope ReadScope
	// Limit 是 1 到 50 的页大小。
	Limit int
	// Cursor 是上一页返回的短时不透明续页令牌。
	Cursor string
}

// MessagePage 保存一页邮件及其可选续页令牌。
type MessagePage struct {
	// Messages 按收件时间降序排列。
	Messages []MessageSummary
	// NextCursor 为空表示本次查询没有下一页。
	NextCursor string
}

// MessageAttachment 是仅供后续安全内容服务使用的附件元数据。
type MessageAttachment struct {
	// Name 是未经浏览器信任的原始附件名。
	Name string
	// MediaType 是邮件声明的内容类型。
	MediaType string
	// Size 是附件字节大小。
	Size int64
	// Reference 是绑定邮箱和邮件的短时不透明 Blob 引用。
	Reference string
}

// MessageDetail 是不含原始 HTML、账号 ID 或 Blob ID 的结构化详情。
type MessageDetail struct {
	// Summary 是当前邮件的列表元数据。
	Summary MessageSummary
	// To 是邮件的 To 收件人。
	To []MailAddress
	// Cc 是邮件的 Cc 收件人。
	Cc []MailAddress
	// Text 是有界纯文本正文。
	Text string
	// TextTruncated 表示原正文超过读取上限。
	TextTruncated bool
	// HasHTML 表示邮件包含需由 SEC-01 单独处理的 HTML 部分。
	HasHTML bool
	// Attachments 是邮件附件的元数据和受限引用。
	Attachments []MessageAttachment
}

// ReadAdapter 只在已获授权的控制面邮箱范围内提供邮件读取。
type ReadAdapter interface {
	// ListMessages 核对邮箱映射后读取一个固定排序的邮件页。
	ListMessages(context.Context, ListMessagesQuery) (MessagePage, error)
	// GetMessage 核对邮箱映射后按不透明引用读取一封邮件。
	GetMessage(context.Context, ReadScope, string) (MessageDetail, error)
}
