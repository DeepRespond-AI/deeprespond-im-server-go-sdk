package drim

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Error 是 SDK 返回的错误：服务端的错误（{ "error": { code, message, details, request_id } }）和 SDK 自己的错误
// （《2.接口设计》第 4 节）。用 errors.As(err, &e) 取得；按 Code 和 Details 判断，不解析 Message。
type Error struct {
	StatusCode int            // HTTP 状态码；SDK 自己的错误为 0（invalid_response 为实际的状态码）
	Code       string         // 服务端的错误码，或 SDK 的本地错误码（CodeNetworkError 等）
	Message    string         // 服务端的说明，只用于日志
	Details    map[string]any // 原样；没有时为 nil
	RequestID  string         // 错误体中的 request_id，没有时为响应头 X-Request-ID，都没有时为 SDK 发出的
	RetryAfter time.Duration  // 响应头 Retry-After
	Op         string         // 接口名，如 users.create
	Attempts   int            // 这次调用一共尝试了几次
	// Retried 为真表示这个错误来自自动重试的尝试，而之前的尝试结果不明（可能已经执行）：
	// already_exists、not_found、version_conflict 时可以按“前一次尝试已经执行”处理（《1.功能概述文档》第 6.1 节）。
	Retried bool
	Err     error // 底层错误（网络错误、context.Canceled、context.DeadlineExceeded 等）
}

// 服务端的错误码（《1.功能概述文档》第 5 节）。以后新增的错误码照常放在 Error.Code 中。
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeUnauthenticated    = "unauthenticated"
	CodeInvalidClient      = "invalid_client"
	CodePermissionDenied   = "permission_denied"
	CodeIPNotAllowed       = "ip_not_allowed"
	CodeTenantUnavailable  = "tenant_unavailable"
	CodeAppUnavailable     = "app_unavailable"
	CodeUserDisabled       = "user_disabled"
	CodeNotGroupMember     = "not_group_member"
	CodeGroupDisabled      = "group_disabled"
	CodeChatroomDisabled   = "chatroom_disabled"
	CodeMessageRejected    = "message_rejected"
	CodeContentRejected    = "content_rejected"
	CodeFileBlocked        = "file_blocked"
	CodeNotFound           = "not_found"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeAlreadyExists      = "already_exists"
	CodeConflict           = "conflict"
	CodeUnavailable        = "unavailable"
	CodeVersionConflict    = "version_conflict"
	CodeLimitExceeded      = "limit_exceeded"
	CodeFileExpired        = "file_expired"
	CodePayloadTooLarge    = "payload_too_large" // 也是 SDK 的本地错误（StatusCode 为 0）
	CodeRateLimited        = "rate_limited"
	CodeInternal           = "internal"
	CodeNetworkError       = "network_error"      // 连接失败、连接中断、TLS 错误
	CodeTimeout            = "timeout"            // 尝试超时或 ctx 的截止时间已到
	CodeCanceled           = "canceled"           // ctx 被取消
	CodeInvalidParams      = "invalid_params"     // 本地检查不通过，Details["field"] 指出参数
	CodeInvalidResponse    = "invalid_response"   // 响应不是预期的 JSON，Details["body_prefix"] 为响应体的前 512 字节
	CodeClientUnavailable  = "client_unavailable" // 客户端已关闭、已永久失效，或处于换取令牌失败后的冷却期
	reasonAppDeleted       = "app_deleted"
	reasonTenantClosed     = "tenant_closed"
	maxBodyPrefix          = 512
	defaultRateLimitedWait = time.Second
)

// Reason 返回 details.reason，没有时为空。
func (e *Error) Reason() string {
	if e == nil || e.Details == nil {
		return ""
	}
	s, _ := e.Details["reason"].(string)
	return s
}

// Error 如 "users.create: 409 already_exists: 用户名已被占用 (request_id=...)"，不含请求体和令牌。
func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.StatusCode != 0 {
		b.WriteString(strconv.Itoa(e.StatusCode))
		b.WriteByte(' ')
	}
	b.WriteString(e.Code)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	var extras []string
	if r := e.Reason(); r != "" {
		extras = append(extras, "reason="+r)
	}
	if f, ok := e.Details["field"].(string); ok && f != "" {
		extras = append(extras, "field="+f)
	}
	if e.RequestID != "" {
		extras = append(extras, "request_id="+e.RequestID)
	}
	if e.Retried {
		extras = append(extras, "retried")
	}
	if len(extras) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(extras, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// Unwrap 返回底层错误：超时为 context.DeadlineExceeded（ctx 的截止时间已到时），取消为 context.Canceled。
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) temporary() bool {
	switch e.Code {
	case CodeNetworkError, CodeTimeout, CodeInternal, CodeRateLimited:
		return true
	}
	return e.StatusCode >= 500
}

func (e *Error) cause() string {
	s, _ := e.Details["cause"].(string)
	return s
}

// IsCode 判断 err 是不是错误码为 code 的 *Error。
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// IsReason 判断 err 是不是错误码为 code、details.reason 为 reason 的 *Error。
func IsReason(err error, code, reason string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code && e.Reason() == reason
}

// IsTemporary 网络错误、超时、internal、5xx、rate_limited：稍后可以再试。
func IsTemporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.temporary()
}

// IsPermanentlyUnavailable 应用已删除、租户已注销（unauthenticated 的 app_deleted、tenant_closed），
// 以及因此永久失效的客户端返回的 client_unavailable：应清除保存的凭据。
func IsPermanentlyUnavailable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	reason := e.Reason()
	permanent := reason == reasonAppDeleted || reason == reasonTenantClosed
	if e.Code == CodeUnauthenticated {
		return permanent
	}
	return e.Code == CodeClientUnavailable && e.cause() == CodeUnauthenticated && permanent
}

// IsServiceUnavailable 服务暂停或只读：tenant_unavailable、app_unavailable，以及冷却期中最初为二者的 client_unavailable。
func IsServiceUnavailable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case CodeTenantUnavailable, CodeAppUnavailable:
		return true
	case CodeClientUnavailable:
		cause := e.cause()
		return cause == CodeTenantUnavailable || cause == CodeAppUnavailable
	}
	return false
}

func newError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func invalidParams(field, message string) *Error {
	e := newError(CodeInvalidParams, message)
	if field != "" {
		e.Details = map[string]any{"field": field}
	}
	return e
}

// parseRetryAfter Retry-After 为整数秒（服务端的约定）；也接受小数和 HTTP 日期。
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// errorFromResponse 把非 2xx 的响应转为错误；不是标准的错误体的（如代理返回的 HTML）为 invalid_response。
func errorFromResponse(status int, header http.Header, body []byte, requestID string) *Error {
	wait := parseRetryAfter(header.Get("Retry-After"), time.Now())
	if id := header.Get("X-Request-ID"); id != "" {
		requestID = id
	}
	var envelope struct {
		Error *struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			Details   map[string]any `json:"details"`
			RequestID string         `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Code != "" {
		e := envelope.Error
		id := requestID
		if e.RequestID != "" {
			id = e.RequestID
		}
		return &Error{
			StatusCode: status,
			Code:       e.Code,
			Message:    e.Message,
			Details:    e.Details,
			RequestID:  id,
			RetryAfter: wait,
		}
	}
	return &Error{
		StatusCode: status,
		Code:       CodeInvalidResponse,
		Message:    fmt.Sprintf("响应不是标准的错误体（HTTP %d）", status),
		Details:    map[string]any{"body_prefix": bodyPrefix(body)},
		RequestID:  requestID,
		RetryAfter: wait,
	}
}

func bodyPrefix(body []byte) string {
	if len(body) > maxBodyPrefix {
		body = body[:maxBodyPrefix]
	}
	return strings.ToValidUTF8(string(body), string(utf8.RuneError))
}

// BatchError 由批量结果的 Err() 返回：有项失败（请求本身成功了，不是 *Error）。
type BatchError struct {
	Failed []ItemError
}

func (e *BatchError) Error() string {
	seen := map[string]bool{}
	var codes []string
	for _, item := range e.Failed {
		if !seen[item.Code] {
			seen[item.Code] = true
			codes = append(codes, item.Code)
		}
	}
	return fmt.Sprintf("%d 项失败：%s", len(e.Failed), strings.Join(codes, "、"))
}
