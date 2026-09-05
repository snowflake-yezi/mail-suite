package stalwart

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/mailcore"
)

const (
	maximumResponseBytes = 1 << 20
	metadataPrefix       = "mail-suite:v1"

	// ErrorCodeAuthentication 表示 Stalwart 拒绝内部管理身份。
	ErrorCodeAuthentication = "MAIL_CORE_AUTHENTICATION_FAILED"
	// ErrorCodeRateLimited 表示 Stalwart 明确要求调用方稍后重试。
	ErrorCodeRateLimited = "MAIL_CORE_RATE_LIMITED"
	// ErrorCodeUnavailable 表示 Stalwart 或其依赖暂时不可用。
	ErrorCodeUnavailable = "MAIL_CORE_UNAVAILABLE"
	// ErrorCodeProtocolInvalid 表示锁定版本没有返回 adapter 所需协议结构。
	ErrorCodeProtocolInvalid = "MAIL_CORE_PROTOCOL_INVALID"
	// ErrorCodeDomainMissing 表示邮箱所属邮件域尚未存在于 Stalwart。
	ErrorCodeDomainMissing = "MAIL_CORE_DOMAIN_MISSING"
	// ErrorCodeResourceConflict 表示目标地址已被其他控制资源占用。
	ErrorCodeResourceConflict = "MAIL_CORE_RESOURCE_CONFLICT"
	// ErrorCodeConfiguration 表示 adapter 收到不符合稳定命令契约的输入。
	ErrorCodeConfiguration = "MAIL_CORE_CONFIGURATION_INVALID"
)

var _ mailcore.Adapter = (*Adapter)(nil)

// Adapter 使用 Stalwart 自定义 JMAP 管理方法确保并观测邮箱账号。
type Adapter struct {
	endpoint      *url.URL
	adminUsername string
	adminPassword string
	mailboxKey    []byte
	httpClient    *http.Client
	now           func() time.Time
}

// New 创建只信任已校验 HTTPS origin 且禁止重定向的 Stalwart adapter。
func New(config Config) (*Adapter, error) {
	if config.Endpoint == nil || config.AdminUsername == "" || config.AdminPassword == "" || len(config.MailboxKey) != 32 ||
		config.RootCAs == nil ||
		config.RequestTimeout <= 0 || config.RequestTimeout > maximumRequestTimeout {
		return nil, errors.New("stalwart adapter 配置无效")
	}
	endpoint, err := parseEndpoint(config.Endpoint.String())
	if err != nil {
		return nil, errors.New("stalwart adapter 配置无效")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    config.RootCAs,
	}
	return &Adapter{
		endpoint:      endpoint,
		adminUsername: config.AdminUsername,
		adminPassword: config.AdminPassword,
		mailboxKey:    append([]byte(nil), config.MailboxKey...),
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   config.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}, nil
}

// Check 验证内部 TLS、认证和 Stalwart JMAP session 能力是否可用。
func (adapter *Adapter) Check(ctx context.Context) error {
	session, err := adapter.fetchSession(ctx)
	if err != nil {
		return err
	}
	_, err = adapter.queryIDs(ctx, session.APIPath, "x:Domain", map[string]any{})
	if _, code := mailcore.ClassifyError(err); code == ErrorCodeProtocolInvalid {
		return protocolError()
	}
	return err
}

// EnsureMailbox 幂等创建或推进由控制面元数据标记的 Stalwart 账号。
func (adapter *Adapter) EnsureMailbox(
	ctx context.Context,
	command mailcore.EnsureMailboxCommand,
) error {
	localPart, domainName, err := validateEnsureCommand(command)
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithDeadline(ctx, command.Deadline)
	defer cancel()
	session, err := adapter.fetchSession(requestContext)
	if err != nil {
		return err
	}
	domain, found, err := adapter.findDomain(requestContext, session.APIPath, domainName)
	if err != nil {
		return err
	}
	if !found {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeDomainMissing)
	}
	account, found, err := adapter.findAccount(requestContext, session.APIPath, localPart, domain.ID)
	if err != nil {
		return err
	}
	if found {
		return adapter.ensureExistingAccount(requestContext, session.APIPath, account, command)
	}
	return adapter.createAccount(requestContext, session.APIPath, localPart, domain.ID, command)
}

// InspectMailbox 从 Stalwart 重新读取实际账号状态和控制面元数据。
func (adapter *Adapter) InspectMailbox(
	ctx context.Context,
	query mailcore.InspectMailboxQuery,
) (mailcore.ObservedMailbox, error) {
	localPart, domainName, err := validateInspectQuery(query)
	if err != nil {
		return mailcore.ObservedMailbox{}, err
	}
	requestContext, cancel := context.WithDeadline(ctx, query.Deadline)
	defer cancel()
	session, err := adapter.fetchSession(requestContext)
	if err != nil {
		return mailcore.ObservedMailbox{}, err
	}
	domain, found, err := adapter.findDomain(requestContext, session.APIPath, domainName)
	if err != nil {
		return mailcore.ObservedMailbox{}, err
	}
	if !found {
		return absentObservation(query.MailboxID, adapter.now()), nil
	}
	account, found, err := adapter.findAccount(requestContext, session.APIPath, localPart, domain.ID)
	if err != nil {
		return mailcore.ObservedMailbox{}, err
	}
	if !found {
		return absentObservation(query.MailboxID, adapter.now()), nil
	}
	metadata, err := parseMetadata(account.Description)
	if err != nil || metadata.MailboxID != query.MailboxID || metadata.DomainID != query.DomainID {
		return mailcore.ObservedMailbox{}, mailcore.NewError(
			mailcore.ErrorClassPermanent,
			ErrorCodeResourceConflict,
		)
	}
	return mailcore.ObservedMailbox{
		MailboxID:         query.MailboxID,
		Status:            mailcore.MailboxStatusActive,
		Revision:          metadata.Revision,
		ConfigurationHash: metadata.ConfigurationHash,
		InspectedAt:       adapter.now().UTC(),
	}, nil
}

// session 保存已验证为同 origin 的 JMAP API 路径。
type session struct {
	APIPath string
}

// fetchSession 获取管理会话并拒绝把后续请求重定向到其他 origin。
func (adapter *Adapter) fetchSession(ctx context.Context) (session, error) {
	var response struct {
		APIURL       string                     `json:"apiUrl"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := adapter.requestJSON(ctx, http.MethodGet, "/jmap/session", nil, false, &response); err != nil {
		return session{}, err
	}
	if _, ok := response.Capabilities["urn:ietf:params:jmap:core"]; !ok {
		return session{}, protocolError()
	}
	apiURL, err := url.Parse(response.APIURL)
	if err != nil || !apiURL.IsAbs() || apiURL.Scheme != adapter.endpoint.Scheme ||
		apiURL.Host != adapter.endpoint.Host || apiURL.User != nil || apiURL.RawQuery != "" ||
		apiURL.Fragment != "" || !strings.HasPrefix(apiURL.Path, "/") {
		return session{}, protocolError()
	}
	return session{APIPath: apiURL.Path}, nil
}

// domainObject 是 Stalwart Domain/get 返回的最小稳定字段集合。
type domainObject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// accountObject 是 Stalwart Account/get 返回的最小稳定字段集合。
type accountObject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DomainID    string `json:"domainId"`
	Description string `json:"description"`
}

// findDomain 按规范化域名查询唯一 Stalwart domain。
func (adapter *Adapter) findDomain(
	ctx context.Context,
	apiPath string,
	name string,
) (domainObject, bool, error) {
	ids, err := adapter.queryIDs(ctx, apiPath, "x:Domain", map[string]any{"name": name})
	if err != nil || len(ids) == 0 {
		return domainObject{}, false, err
	}
	if len(ids) != 1 {
		return domainObject{}, false, protocolError()
	}
	var domains []domainObject
	if err = adapter.getObjects(ctx, apiPath, "x:Domain", ids, []string{"id", "name"}, &domains); err != nil {
		return domainObject{}, false, err
	}
	if len(domains) != 1 || domains[0].ID == "" || !strings.EqualFold(domains[0].Name, name) {
		return domainObject{}, false, protocolError()
	}
	return domains[0], true, nil
}

// findAccount 按本地部分和 Stalwart domain ID 查询唯一账号。
func (adapter *Adapter) findAccount(
	ctx context.Context,
	apiPath string,
	localPart string,
	domainID string,
) (accountObject, bool, error) {
	ids, err := adapter.queryIDs(ctx, apiPath, "x:Account", map[string]any{
		"name":     localPart,
		"domainId": domainID,
	})
	if err != nil || len(ids) == 0 {
		return accountObject{}, false, err
	}
	if len(ids) != 1 {
		return accountObject{}, false, protocolError()
	}
	var accounts []accountObject
	properties := []string{"id", "name", "domainId", "description"}
	if err = adapter.getObjects(ctx, apiPath, "x:Account", ids, properties, &accounts); err != nil {
		return accountObject{}, false, err
	}
	if len(accounts) != 1 || accounts[0].ID == "" || accounts[0].Name != localPart ||
		accounts[0].DomainID != domainID {
		return accountObject{}, false, protocolError()
	}
	return accounts[0], true, nil
}

// queryIDs 调用单页精确查询，并拒绝超出唯一性预期的结果。
func (adapter *Adapter) queryIDs(
	ctx context.Context,
	apiPath string,
	objectName string,
	filter map[string]any,
) ([]string, error) {
	var result struct {
		IDs []string `json:"ids"`
	}
	err := adapter.call(ctx, apiPath, objectName+"/query", map[string]any{
		"filter":         filter,
		"limit":          2,
		"calculateTotal": true,
	}, false, &result)
	if err != nil {
		return nil, err
	}
	for _, id := range result.IDs {
		if strings.TrimSpace(id) == "" {
			return nil, protocolError()
		}
	}
	return result.IDs, nil
}

// getObjects 读取 query 返回的固定 ID，并要求 list 完整存在。
func (adapter *Adapter) getObjects(
	ctx context.Context,
	apiPath string,
	objectName string,
	ids []string,
	properties []string,
	target any,
) error {
	var result struct {
		List json.RawMessage `json:"list"`
	}
	if err := adapter.call(ctx, apiPath, objectName+"/get", map[string]any{
		"ids":        ids,
		"properties": properties,
	}, false, &result); err != nil {
		return err
	}
	if len(result.List) == 0 || json.Unmarshal(result.List, target) != nil {
		return protocolError()
	}
	return nil
}

// createAccount 创建带控制面元数据和派生内部凭据的 Stalwart User 账号。
func (adapter *Adapter) createAccount(
	ctx context.Context,
	apiPath string,
	localPart string,
	domainID string,
	command mailcore.EnsureMailboxCommand,
) error {
	metadata := formatMetadata(command)
	fields := map[string]any{
		"@type":       "User",
		"name":        localPart,
		"domainId":    domainID,
		"description": metadata,
		"credentials": map[string]any{
			"0": map[string]any{
				"@type":  "Password",
				"secret": adapter.mailboxPassword(command.MailboxID, command.DesiredRevision),
			},
		},
	}
	var result setResult
	err := adapter.call(ctx, apiPath, "x:Account/set", map[string]any{
		"create": map[string]any{"mail-suite": fields},
	}, true, &result)
	if err != nil {
		return err
	}
	if created, ok := result.Created["mail-suite"]; ok && created.ID != "" {
		return nil
	}
	if _, ok := result.NotCreated["mail-suite"]; ok {
		account, found, lookupErr := adapter.findAccount(ctx, apiPath, localPart, domainID)
		if lookupErr != nil {
			return lookupErr
		}
		if found {
			return adapter.ensureExistingAccount(ctx, apiPath, account, command)
		}
		return classifySetError(result.NotCreated["mail-suite"].Type)
	}
	return protocolError()
}

// ensureExistingAccount 验证资源所有权，并仅向前推进控制面 revision。
func (adapter *Adapter) ensureExistingAccount(
	ctx context.Context,
	apiPath string,
	account accountObject,
	command mailcore.EnsureMailboxCommand,
) error {
	metadata, err := parseMetadata(account.Description)
	if err != nil || metadata.MailboxID != command.MailboxID || metadata.DomainID != command.DomainID {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeResourceConflict)
	}
	if metadata.Revision > command.DesiredRevision {
		return nil
	}
	if metadata.Revision == command.DesiredRevision &&
		metadata.ConfigurationHash != command.ConfigurationHash {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeResourceConflict)
	}
	if metadata.Revision == command.DesiredRevision {
		return nil
	}
	return adapter.updateAccount(ctx, apiPath, account.ID, command)
}

// updateAccount 原子替换受控元数据和派生内部凭据。
func (adapter *Adapter) updateAccount(
	ctx context.Context,
	apiPath string,
	accountID string,
	command mailcore.EnsureMailboxCommand,
) error {
	var result setResult
	err := adapter.call(ctx, apiPath, "x:Account/set", map[string]any{
		"update": map[string]any{
			accountID: map[string]any{
				"description": formatMetadata(command),
				"credentials": map[string]any{
					"0": map[string]any{
						"@type":  "Password",
						"secret": adapter.mailboxPassword(command.MailboxID, command.DesiredRevision),
					},
				},
			},
		},
	}, true, &result)
	if err != nil {
		return err
	}
	if _, ok := result.Updated[accountID]; ok {
		return nil
	}
	if setFailure, ok := result.NotUpdated[accountID]; ok {
		return classifySetError(setFailure.Type)
	}
	return protocolError()
}

// setResult 表示 Stalwart 自定义 JMAP set 的成功与逐对象失败结果。
type setResult struct {
	Created    map[string]createdObject   `json:"created"`
	Updated    map[string]json.RawMessage `json:"updated"`
	NotCreated map[string]setError        `json:"notCreated"`
	NotUpdated map[string]setError        `json:"notUpdated"`
}

// createdObject 保存 set create 返回的不可变 Stalwart 对象 ID。
type createdObject struct {
	ID string `json:"id"`
}

// setError 只读取稳定错误类型，忽略可能包含敏感输入的描述字段。
type setError struct {
	Type string `json:"type"`
}

// call 构造单个 Stalwart JMAP 方法调用并验证响应名称与 call ID。
func (adapter *Adapter) call(
	ctx context.Context,
	apiPath string,
	methodName string,
	arguments any,
	mutation bool,
	target any,
) error {
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"},
		"methodCalls": []any{
			[]any{methodName, arguments, "c0"},
		},
	}
	var envelope struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := adapter.requestJSON(ctx, http.MethodPost, apiPath, body, mutation, &envelope); err != nil {
		return err
	}
	if len(envelope.MethodResponses) != 1 {
		return protocolError()
	}
	var responseParts []json.RawMessage
	if err := json.Unmarshal(envelope.MethodResponses[0], &responseParts); err != nil || len(responseParts) != 3 {
		return protocolError()
	}
	var responseName string
	var callID string
	if json.Unmarshal(responseParts[0], &responseName) != nil ||
		json.Unmarshal(responseParts[2], &callID) != nil || callID != "c0" {
		return protocolError()
	}
	if responseName == "error" {
		var methodError setError
		if json.Unmarshal(responseParts[1], &methodError) != nil {
			return protocolError()
		}
		return classifyMethodError(methodError.Type, mutation)
	}
	if responseName != methodName || json.Unmarshal(responseParts[1], target) != nil {
		return protocolError()
	}
	return nil
}

// requestJSON 执行单次有界 JSON 请求，并按是否可能产生副作用映射失败。
func (adapter *Adapter) requestJSON(
	ctx context.Context,
	method string,
	path string,
	body any,
	mutation bool,
	target any,
) error {
	requestURL := cloneURL(adapter.endpoint)
	requestURL.Path = path
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), requestBody)
	if err != nil {
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	request.SetBasicAuth(adapter.adminUsername, adapter.adminPassword)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := adapter.httpClient.Do(request)
	if err != nil {
		return transportError(ctx, mutation)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError(response.StatusCode, mutation)
	}
	limited := io.LimitReader(response.Body, maximumResponseBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumResponseBytes ||
		json.Unmarshal(encoded, target) != nil {
		return mailcore.NewError(uncertainClass(mutation), ErrorCodeProtocolInvalid)
	}
	return nil
}

// mailboxMetadata 是写入 Stalwart description 的不可逆控制面观测证据。
type mailboxMetadata struct {
	MailboxID         uuid.UUID
	DomainID          uuid.UUID
	OperationID       uuid.UUID
	Revision          int64
	ConfigurationHash [32]byte
}

// formatMetadata 生成不包含地址或凭据的固定版本控制面标记。
func formatMetadata(command mailcore.EnsureMailboxCommand) string {
	return fmt.Sprintf(
		"%s:%s:%s:%s:%d:%s",
		metadataPrefix,
		command.MailboxID,
		command.DomainID,
		command.OperationID,
		command.DesiredRevision,
		hex.EncodeToString(command.ConfigurationHash[:]),
	)
}

// parseMetadata 严格解析 adapter 自有 description，拒绝旧版或人工占位内容。
func parseMetadata(raw string) (mailboxMetadata, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 7 || strings.Join(parts[:2], ":") != metadataPrefix {
		return mailboxMetadata{}, errors.New("stalwart 邮箱控制元数据无效")
	}
	mailboxID, mailboxErr := uuid.Parse(parts[2])
	domainID, domainErr := uuid.Parse(parts[3])
	operationID, operationErr := uuid.Parse(parts[4])
	revision, revisionErr := strconv.ParseInt(parts[5], 10, 64)
	configurationHash, hashErr := hex.DecodeString(parts[6])
	if mailboxErr != nil || domainErr != nil || operationErr != nil || revisionErr != nil || revision <= 0 ||
		hashErr != nil || len(configurationHash) != sha256.Size {
		return mailboxMetadata{}, errors.New("stalwart 邮箱控制元数据无效")
	}
	var fixedHash [32]byte
	copy(fixedHash[:], configurationHash)
	return mailboxMetadata{
		MailboxID:         mailboxID,
		DomainID:          domainID,
		OperationID:       operationID,
		Revision:          revision,
		ConfigurationHash: fixedHash,
	}, nil
}

// mailboxPassword 从不可导出的部署密钥和控制面身份派生测试环境内部凭据。
func (adapter *Adapter) mailboxPassword(mailboxID uuid.UUID, revision int64) string {
	digest := hmac.New(sha256.New, adapter.mailboxKey)
	_, _ = digest.Write(mailboxID[:])
	var encodedRevision [8]byte
	binary.BigEndian.PutUint64(encodedRevision[:], uint64(revision))
	_, _ = digest.Write(encodedRevision[:])
	return "ms1_" + base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

// validateEnsureCommand 拒绝缺少身份、版本、摘要或有效 deadline 的副作用请求。
func validateEnsureCommand(command mailcore.EnsureMailboxCommand) (string, string, error) {
	if command.OperationID == uuid.Nil || command.MailboxID == uuid.Nil || command.DomainID == uuid.Nil ||
		command.DesiredRevision <= 0 || command.PayloadVersion != mailcore.PayloadVersion ||
		command.Deadline.IsZero() {
		return "", "", mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	return splitAddress(command.Address)
}

// validateInspectQuery 拒绝缺少资源身份、版本、摘要或有效 deadline 的观测请求。
func validateInspectQuery(query mailcore.InspectMailboxQuery) (string, string, error) {
	if query.MailboxID == uuid.Nil || query.DomainID == uuid.Nil || query.DesiredRevision <= 0 ||
		query.Deadline.IsZero() {
		return "", "", mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	return splitAddress(query.Address)
}

// splitAddress 接受 repository 已规范化的单一 ASCII local@domain 地址。
func splitAddress(address string) (string, string, error) {
	if address != strings.ToLower(strings.TrimSpace(address)) || strings.Count(address, "@") != 1 {
		return "", "", mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	localPart, domainName, _ := strings.Cut(address, "@")
	if localPart == "" || domainName == "" || len(address) > 320 ||
		strings.IndexFunc(address, func(value rune) bool {
			return value < 0x21 || value > 0x7e
		}) >= 0 {
		return "", "", mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeConfiguration)
	}
	return localPart, domainName, nil
}

// absentObservation 返回带实际检查时间的明确不存在结果。
func absentObservation(mailboxID uuid.UUID, inspectedAt time.Time) mailcore.ObservedMailbox {
	return mailcore.ObservedMailbox{
		MailboxID:   mailboxID,
		Status:      mailcore.MailboxStatusAbsent,
		InspectedAt: inspectedAt.UTC(),
	}
}

// classifySetError 将逐对象失败映射为稳定且不包含服务端描述的错误。
func classifySetError(errorType string) error {
	switch errorType {
	case "rateLimit":
		return mailcore.NewError(mailcore.ErrorClassRetryable, ErrorCodeRateLimited)
	case "serverUnavailable":
		return mailcore.NewError(mailcore.ErrorClassRetryable, ErrorCodeUnavailable)
	case "forbidden":
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeAuthentication)
	default:
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeResourceConflict)
	}
}

// classifyMethodError 将 JMAP 方法级错误按副作用不确定性归一化。
func classifyMethodError(errorType string, mutation bool) error {
	switch errorType {
	case "rateLimit":
		return mailcore.NewError(mailcore.ErrorClassRetryable, ErrorCodeRateLimited)
	case "forbidden", "accountNotFound":
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeAuthentication)
	case "serverUnavailable":
		return mailcore.NewError(uncertainClass(mutation), ErrorCodeUnavailable)
	default:
		return mailcore.NewError(uncertainClass(mutation), ErrorCodeProtocolInvalid)
	}
}

// statusError 将 HTTP 状态映射为不会泄漏响应正文的稳定错误。
func statusError(status int, mutation bool) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeAuthentication)
	case http.StatusTooManyRequests:
		return mailcore.NewError(mailcore.ErrorClassRetryable, ErrorCodeRateLimited)
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return mailcore.NewError(uncertainClass(mutation), ErrorCodeUnavailable)
	default:
		if status >= 500 {
			return mailcore.NewError(uncertainClass(mutation), ErrorCodeUnavailable)
		}
		return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeProtocolInvalid)
	}
}

// transportError 保留调用取消语义，并区分只读请求和可能已提交的 mutation。
func transportError(ctx context.Context, mutation bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return mailcore.NewError(uncertainClass(mutation), ErrorCodeUnavailable)
}

// uncertainClass 对 mutation 使用 unknown，对幂等只读请求使用 retryable。
func uncertainClass(mutation bool) mailcore.ErrorClass {
	if mutation {
		return mailcore.ErrorClassUnknown
	}
	return mailcore.ErrorClassRetryable
}

// protocolError 返回不含底层响应内容的稳定协议错误。
func protocolError() error {
	return mailcore.NewError(mailcore.ErrorClassPermanent, ErrorCodeProtocolInvalid)
}

// cloneURL 防止调用方在 adapter 构造后修改共享 URL。
func cloneURL(source *url.URL) *url.URL {
	copy := *source
	return &copy
}
