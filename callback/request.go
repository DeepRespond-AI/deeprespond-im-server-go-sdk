package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Request 是一个回调请求（《2.接口设计》第 9.1 节）。
type Request struct {
	Kind       string    // event、hook、verify（以签名保护的请求体为准，不用请求头 X-IM-Kind）
	RequestID  string    // 每次发送都是新的，重试也不同
	App        string    // AppKey：org_name#app_name；多个应用共用一个接收端时按它区分
	EndpointID string    // 回调地址的 ID
	SentAt     time.Time // 发送的时刻
	Events     []*Event  // kind 为 event 时
	Hook       string    // kind 为 hook 时：message.before_send 等
	TimeoutMs  int       // kind 为 hook 时：服务端本次实际等待的毫秒数
	Test       bool      // kind 为 hook 时；事件的测试标记在每个 Event 中
	Data       json.RawMessage
	Challenge  string // kind 为 verify 时
	Raw        []byte // 原始请求体
}

// Event 是一个事件。Data 与请求体中的逐字节相同；Decode 按 Type 解析为对应的类型。
type Event struct {
	ID         string    // 事件 ID：同一事件多次到达（重试、重新投递）时不变，按它去重
	Type       string    // 如 message.sent
	OccurredAt time.Time // 发生的时刻
	Attempt    int       // 第几次发送，从 1 开始（重新投递后重新计数）
	Test       bool      // 控制台“测试发送”产生的事件
	Data       json.RawMessage
	Raw        json.RawMessage // 这个事件在请求体中的原始 JSON
}

// EventBase 是每种事件的数据类型都嵌入的公共字段。
type EventBase struct {
	// Origin 谁引起的：client、server、system、platform 等；在线状态变化为 nil。
	Origin *string `json:"origin"`
	// Truncated data 超过 70 KB 被截短：对象和数组字段、原文超过 512 字节的值和 body、ext 等文本字段被去掉（为零值），
	// 需要时按 ID 调用 OpenAPI 读取。
	Truncated bool `json:"truncated"`
	// Raw 是 data 的原始 JSON，SDK 没有解析的字段从中读取。
	Raw json.RawMessage `json:"-"`
}

// HookBase 是同步回调的请求类型都嵌入的公共字段。
type HookBase struct {
	Test    bool            `json:"-"` // 控制台“测试发送”产生的请求
	Timeout time.Duration   `json:"-"` // 服务端给出的等待时间（timeout_ms）
	Raw     json.RawMessage `json:"-"` // 原始的 data
}

// ErrUnknownEventType 是 SDK 还不认识的事件类型（服务端新增的）。
var ErrUnknownEventType = errors.New("callback: 不认识的事件类型")

// Parse 解析请求体（不校验签名，先用 Verify）。不是合法的回调请求时返回错误。
func Parse(body []byte) (*Request, error) {
	var envelope struct {
		Kind       string          `json:"kind"`
		RequestID  string          `json:"request_id"`
		App        string          `json:"app"`
		EndpointID string          `json:"endpoint_id"`
		SentAt     string          `json:"sent_at"`
		Events     json.RawMessage `json:"events"`
		Hook       string          `json:"hook"`
		TimeoutMs  int             `json:"timeout_ms"`
		Test       bool            `json:"test"`
		Data       json.RawMessage `json:"data"`
		Challenge  *string         `json:"challenge"`
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("callback: 请求体不是 JSON 对象")
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("callback: 请求体不是合法的 JSON：%w", err)
	}
	req := &Request{
		Kind:       envelope.Kind,
		RequestID:  envelope.RequestID,
		App:        envelope.App,
		EndpointID: envelope.EndpointID,
		SentAt:     parseTime(envelope.SentAt),
		Raw:        body,
	}
	switch envelope.Kind {
	case "verify":
		if envelope.Challenge == nil {
			return nil, errors.New("callback: URL 验证请求缺少 challenge")
		}
		req.Challenge = *envelope.Challenge
	case "hook":
		if envelope.Hook == "" || !isObject(envelope.Data) {
			return nil, errors.New("callback: 同步回调请求缺少 hook 或 data")
		}
		req.Hook = envelope.Hook
		req.TimeoutMs = envelope.TimeoutMs
		req.Test = envelope.Test
		req.Data = envelope.Data
	case "event":
		var items []json.RawMessage
		if json.Unmarshal(envelope.Events, &items) != nil || items == nil {
			return nil, errors.New("callback: 事件回调请求缺少 events")
		}
		for i, raw := range items {
			var item struct {
				ID         *string         `json:"id"`
				Type       *string         `json:"type"`
				OccurredAt string          `json:"occurred_at"`
				Attempt    int             `json:"attempt"`
				Test       bool            `json:"test"`
				Data       json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &item); err != nil || item.ID == nil || item.Type == nil || *item.ID == "" || *item.Type == "" {
				return nil, fmt.Errorf("callback: 第 %d 个事件缺少 id 或 type", i+1)
			}
			data := item.Data
			if !isObject(data) {
				data = json.RawMessage("{}")
			}
			attempt := item.Attempt
			if attempt <= 0 {
				attempt = 1
			}
			req.Events = append(req.Events, &Event{
				ID: *item.ID, Type: *item.Type, OccurredAt: parseTime(item.OccurredAt), Attempt: attempt, Test: item.Test,
				Data: data, Raw: raw,
			})
		}
	default:
		return nil, fmt.Errorf("callback: 不认识的 kind %q", envelope.Kind)
	}
	return req, nil
}

func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

type requestKey struct{}

// RequestFrom 返回处理函数所在的回调请求的公共信息（不含 Events、Data、Raw）；不在回调处理中时为 nil。
func RequestFrom(ctx context.Context) *Request {
	req, _ := ctx.Value(requestKey{}).(*Request)
	return req
}

func withRequest(ctx context.Context, req *Request) context.Context {
	info := &Request{
		Kind: req.Kind, RequestID: req.RequestID, App: req.App, EndpointID: req.EndpointID, SentAt: req.SentAt,
		Hook: req.Hook, TimeoutMs: req.TimeoutMs, Test: req.Test, Challenge: req.Challenge,
	}
	return context.WithValue(ctx, requestKey{}, info)
}

// cloneRaw 复制原始 JSON（json.Unmarshal 传给 UnmarshalJSON 的切片不能保留）。
func cloneRaw(b []byte) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// nullRaw 把 JSON 的 null 转为 nil。
func nullRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 || string(bytes.TrimSpace(r)) == "null" {
		return nil
	}
	return r
}
