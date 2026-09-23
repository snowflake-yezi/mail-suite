package stalwart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

const (
	mailCapability      = "urn:ietf:params:jmap:mail"
	coreCapability      = "urn:ietf:params:jmap:core"
	maximumPageSize     = 50
	maximumPagePosition = 10000
	maximumTextBytes    = 64 << 10
	maximumAttachments  = 100
)

var _ mailcore.ReadAdapter = (*Adapter)(nil)

// mailboxSession 保存已核对邮箱凭据对应的 JMAP 路径与账号标识。
type mailboxSession struct {
	APIPath   string
	AccountID string
	Username  string
	Password  string
}

// emailObject 是 Email/get 返回的受控字段集合，内部标识不会离开 adapter。
type emailObject struct {
	ID            string                 `json:"id"`
	ReceivedAt    string                 `json:"receivedAt"`
	From          []mailcore.MailAddress `json:"from"`
	To            []mailcore.MailAddress `json:"to"`
	Cc            []mailcore.MailAddress `json:"cc"`
	Subject       string                 `json:"subject"`
	Preview       string                 `json:"preview"`
	Size          int64                  `json:"size"`
	HasAttachment bool                   `json:"hasAttachment"`
	TextBody      []emailBodyPart        `json:"textBody"`
	HTMLBody      []emailBodyPart        `json:"htmlBody"`
	BodyValues    map[string]bodyValue   `json:"bodyValues"`
	Attachments   []emailBodyPart        `json:"attachments"`
}

// emailBodyPart 保存正文或附件部分在当前邮件内的引用。
type emailBodyPart struct {
	PartID string `json:"partId"`
	BlobID string `json:"blobId"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
}

// bodyValue 保存按 JMAP 字节上限读取的纯文本正文。
type bodyValue struct {
	Value       string `json:"value"`
	IsTruncated bool   `json:"isTruncated"`
}

// validateReadScope 拒绝缺失身份、无效地址或不受 deadline 约束的读取。
func validateReadScope(scope mailcore.ReadScope) error {
	if scope.MailboxID == uuid.Nil || scope.DomainID == uuid.Nil ||
		scope.Revision <= 0 || scope.Deadline.IsZero() {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	_, _, err := splitAddress(scope.Address)
	return err
}

// openMailboxSession 先用管理身份重新核对映射，再切换到该邮箱的 JMAP 身份。
func (adapter *Adapter) openMailboxSession(ctx context.Context, scope mailcore.ReadScope) (mailboxSession, error) {
	localPart, domainName, _ := splitAddress(scope.Address)
	adminSession, err := adapter.fetchSession(ctx)
	if err != nil {
		return mailboxSession{}, err
	}
	domain, found, err := adapter.findDomain(ctx, adminSession.APIPath, domainName)
	if err != nil {
		return mailboxSession{}, err
	}
	if !found {
		return mailboxSession{}, mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeMailboxNotFound)
	}
	account, found, err := adapter.findAccount(ctx, adminSession.APIPath, localPart, domain.ID)
	if err != nil {
		return mailboxSession{}, err
	}
	if !found {
		return mailboxSession{}, mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeMailboxNotFound)
	}
	metadata, err := parseMetadata(account.Description)
	if err != nil || metadata.MailboxID != scope.MailboxID || metadata.DomainID != scope.DomainID ||
		metadata.Revision != scope.Revision || metadata.ConfigurationHash != scope.ConfigurationHash {
		return mailboxSession{}, mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeResourceConflict)
	}
	password := adapter.mailboxPassword(scope.MailboxID, scope.Revision)
	var response struct {
		APIURL          string                     `json:"apiUrl"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	}
	if err = adapter.requestJSONAs(ctx, http.MethodGet, "/jmap/session", nil, false,
		scope.Address, password, &response); err != nil {
		return mailboxSession{}, err
	}
	if _, ok := response.Capabilities[coreCapability]; !ok {
		return mailboxSession{}, protocolError()
	}
	if _, ok := response.Capabilities[mailCapability]; !ok || response.PrimaryAccounts[mailCapability] == "" {
		return mailboxSession{}, protocolError()
	}
	apiURL, err := url.Parse(response.APIURL)
	if err != nil || !apiURL.IsAbs() || apiURL.Scheme != adapter.endpoint.Scheme ||
		apiURL.Host != adapter.endpoint.Host || apiURL.User != nil || apiURL.RawQuery != "" ||
		apiURL.Fragment != "" || !strings.HasPrefix(apiURL.Path, "/") {
		return mailboxSession{}, protocolError()
	}
	return mailboxSession{
		APIPath: apiURL.Path, AccountID: response.PrimaryAccounts[mailCapability],
		Username: scope.Address, Password: password,
	}, nil
}

// callMailbox 使用邮箱身份和标准 mail capability 调用 JMAP 邮件方法。
func (adapter *Adapter) callMailbox(ctx context.Context, session mailboxSession,
	method string, arguments any, target any) error {
	return adapter.callAs(ctx, session.APIPath, method, arguments, false,
		session.Username, session.Password, []string{coreCapability, mailCapability}, target)
}

// ListMessages 返回当前已核对邮箱中最多 50 封邮件及绑定查询状态的续页游标。
func (adapter *Adapter) ListMessages(ctx context.Context, query mailcore.ListMessagesQuery) (mailcore.MessagePage, error) {
	if err := validateReadScope(query.Scope); err != nil {
		return mailcore.MessagePage{}, err
	}
	if query.Limit < 1 || query.Limit > maximumPageSize {
		return mailcore.MessagePage{}, mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	position := 0
	previousState := ""
	if query.Cursor != "" {
		cursor, err := adapter.openReadToken(query.Cursor, query.Scope, "cursor")
		if err != nil || cursor.Position <= 0 || cursor.Position > maximumPagePosition || cursor.QueryState == "" {
			return mailcore.MessagePage{}, tokenError("cursor")
		}
		position, previousState = cursor.Position, cursor.QueryState
	}
	if position+query.Limit > maximumPagePosition {
		return mailcore.MessagePage{}, tokenError("cursor")
	}
	requestContext, cancel := context.WithDeadline(ctx, query.Scope.Deadline)
	defer cancel()
	session, err := adapter.openMailboxSession(requestContext, query.Scope)
	if err != nil {
		return mailcore.MessagePage{}, err
	}
	var result struct {
		IDs        []string `json:"ids"`
		QueryState string   `json:"queryState"`
		Position   int      `json:"position"`
	}
	err = adapter.callMailbox(requestContext, session, "Email/query", map[string]any{
		"accountId": session.AccountID,
		"filter":    map[string]any{},
		"sort":      []any{map[string]any{"property": "receivedAt", "isAscending": false}},
		"position":  position,
		"limit":     query.Limit + 1,
	}, &result)
	if err != nil {
		return mailcore.MessagePage{}, err
	}
	if result.QueryState == "" || result.Position != position || len(result.IDs) > query.Limit+1 {
		return mailcore.MessagePage{}, protocolError()
	}
	if previousState != "" && previousState != result.QueryState {
		return mailcore.MessagePage{}, tokenError("cursor")
	}
	hasMore := len(result.IDs) > query.Limit
	ids := result.IDs
	if hasMore {
		ids = ids[:query.Limit]
	}
	page := mailcore.MessagePage{Messages: make([]mailcore.MessageSummary, 0, len(ids))}
	if len(ids) > 0 {
		emails, getErr := adapter.getEmails(requestContext, session, ids, summaryProperties(), false)
		if getErr != nil {
			return mailcore.MessagePage{}, getErr
		}
		byID := make(map[string]emailObject, len(emails))
		for _, email := range emails {
			if email.ID == "" || byID[email.ID].ID != "" {
				return mailcore.MessagePage{}, protocolError()
			}
			byID[email.ID] = email
		}
		for _, id := range ids {
			email, ok := byID[id]
			if !ok {
				return mailcore.MessagePage{}, tokenError("cursor")
			}
			summary, summaryErr := adapter.messageSummary(query.Scope, email)
			if summaryErr != nil {
				return mailcore.MessagePage{}, summaryErr
			}
			page.Messages = append(page.Messages, summary)
		}
	}
	if hasMore {
		cursor := adapter.newReadToken(query.Scope, "cursor", cursorLifetime)
		cursor.QueryState = result.QueryState
		cursor.Position = position + len(ids)
		page.NextCursor, err = adapter.sealReadToken(cursor)
		if err != nil {
			return mailcore.MessagePage{}, err
		}
	}
	return page, nil
}

// GetMessage 在准确邮箱映射下读取一封邮件的元数据和有界纯文本正文。
func (adapter *Adapter) GetMessage(ctx context.Context, scope mailcore.ReadScope, opaqueID string) (mailcore.MessageDetail, error) {
	if err := validateReadScope(scope); err != nil {
		return mailcore.MessageDetail{}, err
	}
	token, err := adapter.openReadToken(opaqueID, scope, "message")
	if err != nil || token.MessageID == "" {
		return mailcore.MessageDetail{}, tokenError("message")
	}
	requestContext, cancel := context.WithDeadline(ctx, scope.Deadline)
	defer cancel()
	session, err := adapter.openMailboxSession(requestContext, scope)
	if err != nil {
		return mailcore.MessageDetail{}, err
	}
	emails, err := adapter.getEmails(requestContext, session, []string{token.MessageID},
		append(summaryProperties(), "to", "cc", "textBody", "htmlBody", "attachments", "bodyValues"), true)
	if err != nil {
		return mailcore.MessageDetail{}, err
	}
	if len(emails) == 0 {
		return mailcore.MessageDetail{}, mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeMessageNotFound)
	}
	if len(emails) != 1 || emails[0].ID != token.MessageID {
		return mailcore.MessageDetail{}, protocolError()
	}
	email := emails[0]
	summary, err := adapter.messageSummary(scope, email)
	if err != nil {
		return mailcore.MessageDetail{}, err
	}
	summary.ID = opaqueID
	detail := mailcore.MessageDetail{
		Summary: summary, To: email.To, Cc: email.Cc, HasHTML: len(email.HTMLBody) > 0,
		Attachments: make([]mailcore.MessageAttachment, 0, len(email.Attachments)),
	}
	for _, part := range email.TextBody {
		value, ok := email.BodyValues[part.PartID]
		if part.PartID == "" || !ok {
			return mailcore.MessageDetail{}, protocolError()
		}
		if detail.Text != "" {
			detail.Text += "\n"
		}
		remaining := maximumTextBytes - len(detail.Text)
		if remaining <= 0 {
			detail.TextTruncated = true
			break
		}
		if len(value.Value) > remaining {
			detail.Text += strings.ToValidUTF8(value.Value[:remaining], "")
			detail.TextTruncated = true
			break
		}
		detail.Text += value.Value
		detail.TextTruncated = detail.TextTruncated || value.IsTruncated
	}
	if len(email.Attachments) > maximumAttachments {
		return mailcore.MessageDetail{}, protocolError()
	}
	for _, part := range email.Attachments {
		if part.BlobID == "" || part.Size < 0 {
			return mailcore.MessageDetail{}, protocolError()
		}
		attachmentToken := adapter.newReadToken(scope, "attachment", referenceLifetime)
		attachmentToken.MessageID, attachmentToken.BlobID = email.ID, part.BlobID
		reference, sealErr := adapter.sealReadToken(attachmentToken)
		if sealErr != nil {
			return mailcore.MessageDetail{}, sealErr
		}
		detail.Attachments = append(detail.Attachments, mailcore.MessageAttachment{
			Name: part.Name, MediaType: part.Type, Size: part.Size, Reference: reference,
		})
	}
	return detail, nil
}

// getEmails 读取一个已知 ID 集合并确认缺失列表只包含请求中的 ID。
func (adapter *Adapter) getEmails(ctx context.Context, session mailboxSession, ids []string,
	properties []string, detail bool) ([]emailObject, error) {
	arguments := map[string]any{
		"accountId":  session.AccountID,
		"ids":        ids,
		"properties": properties,
	}
	if detail {
		arguments["bodyProperties"] = []string{"partId", "blobId", "name", "type", "size"}
		arguments["fetchTextBodyValues"] = true
		arguments["fetchHTMLBodyValues"] = false
		arguments["maxBodyValueBytes"] = maximumTextBytes
	}
	var result struct {
		List     []emailObject `json:"list"`
		NotFound []string      `json:"notFound"`
	}
	if err := adapter.callMailbox(ctx, session, "Email/get", arguments, &result); err != nil {
		return nil, err
	}
	if len(result.List)+len(result.NotFound) != len(ids) {
		return nil, protocolError()
	}
	if len(result.NotFound) > 0 && (len(ids) != 1 || result.NotFound[0] != ids[0]) {
		return nil, protocolError()
	}
	return result.List, nil
}

// summaryProperties 返回列表和详情共用的 JMAP 元数据字段。
func summaryProperties() []string {
	return []string{"id", "receivedAt", "from", "subject", "preview", "size", "hasAttachment"}
}

// messageSummary 将原始邮件 ID 加密为当前控制面邮箱的短时引用。
func (adapter *Adapter) messageSummary(scope mailcore.ReadScope, email emailObject) (mailcore.MessageSummary, error) {
	receivedAt, err := time.Parse(time.RFC3339, email.ReceivedAt)
	if email.ID == "" || err != nil || email.Size < 0 {
		return mailcore.MessageSummary{}, protocolError()
	}
	token := adapter.newReadToken(scope, "message", referenceLifetime)
	token.MessageID = email.ID
	opaqueID, err := adapter.sealReadToken(token)
	if err != nil {
		return mailcore.MessageSummary{}, err
	}
	return mailcore.MessageSummary{
		ID: opaqueID, ReceivedAt: receivedAt, From: email.From, Subject: email.Subject,
		Preview: email.Preview, Size: email.Size, HasAttachment: email.HasAttachment,
	}, nil
}
