package drimtest

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

// 回调请求的构造与签名，用于测试自己的接收端。

const (
	// DefaultEndpointID 是构造请求时默认的回调地址 ID。
	DefaultEndpointID = "1840200000000000007"
	// DefaultApp 是构造请求时默认的 AppKey。
	DefaultApp = "1100250925#demo"
)

// SignedRequest 构造带正确签名的回调请求（POST，路径 /）：时间戳为当前时间、随机数为新生成的，
// secrets 为签名用的密钥（轮换期间可以给新旧两个，新的在前）。
func SignedRequest(endpointID string, body []byte, secrets ...string) *http.Request {
	header := SignedHeader(endpointID, body, time.Now(), randomNonce(), secrets...)
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header = header
	return req
}

// SignedHeader 构造回调请求的请求头（可以指定时间戳和随机数，用于测试过期和重放）。
func SignedHeader(endpointID string, body []byte, at time.Time, nonce string, secrets ...string) http.Header {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("User-Agent", "IM-Callback/1")
	header.Set("X-IM-Endpoint-Id", endpointID)
	header.Set("X-IM-Timestamp", timestamp)
	header.Set("X-IM-Nonce", nonce)
	header.Set("X-IM-Signature", callback.SignatureHeader(secrets, timestamp, nonce, body))
	var envelope struct {
		Kind      string `json:"kind"`
		Hook      string `json:"hook"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		header.Set("X-IM-Kind", envelope.Kind)
		header.Set("X-IM-Request-Id", envelope.RequestID)
		if envelope.Hook != "" {
			header.Set("X-IM-Hook", envelope.Hook)
		}
	}
	return header
}

// Event 是 EventRequest 的一个事件。
type Event struct {
	ID         string // 默认生成
	Type       string // 如 message.sent
	Data       any    // json.RawMessage、[]byte、string 原样，其他值编码为 JSON；nil 为 {}
	OccurredAt time.Time
	Attempt    int // 默认 1
	Test       bool
}

// EventRequest 生成事件回调的请求体（地址 ID 为 DefaultEndpointID）。
func EventRequest(events ...Event) []byte {
	return EventRequestFor(DefaultEndpointID, events...)
}

// EventRequestFor 生成指定地址的事件回调请求体。
func EventRequestFor(endpointID string, events ...Event) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"kind":"event",`)
	writeCommon(&buf, endpointID)
	buf.WriteString(`,"events":[`)
	for i, ev := range events {
		if i > 0 {
			buf.WriteByte(',')
		}
		id := ev.ID
		if id == "" {
			id = randomID()
		}
		at := ev.OccurredAt
		if at.IsZero() {
			at = time.Now()
		}
		attempt := ev.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		buf.WriteString(`{"id":` + quote(id) + `,"type":` + quote(ev.Type) + `,"occurred_at":` + quote(formatTime(at)) +
			`,"attempt":` + strconv.Itoa(attempt) + `,"test":` + strconv.FormatBool(ev.Test) + `,"data":`)
		buf.Write(rawOf(ev.Data))
		buf.WriteByte('}')
	}
	buf.WriteString("]}")
	return buf.Bytes()
}

// HookRequest 生成同步回调的请求体（timeout_ms 为 1000）。
func HookRequest(hook string, data any) []byte {
	return HookRequestFor(DefaultEndpointID, hook, 1000, data)
}

// HookRequestFor 生成指定地址和等待时间的同步回调请求体。
func HookRequestFor(endpointID, hook string, timeoutMs int, data any) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"kind":"hook","hook":` + quote(hook) + ",")
	writeCommon(&buf, endpointID)
	buf.WriteString(`,"timeout_ms":` + strconv.Itoa(timeoutMs) + `,"test":false,"data":`)
	buf.Write(rawOf(data))
	buf.WriteByte('}')
	return buf.Bytes()
}

// VerifyRequest 生成 URL 验证的请求体。
func VerifyRequest(challenge string) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"kind":"verify",`)
	writeCommon(&buf, DefaultEndpointID)
	buf.WriteString(`,"challenge":` + quote(challenge) + "}")
	return buf.Bytes()
}

func writeCommon(buf *bytes.Buffer, endpointID string) {
	buf.WriteString(`"request_id":` + quote(randomID()) + `,"app":` + quote(DefaultApp) + `,"endpoint_id":` + quote(endpointID) +
		`,"sent_at":` + quote(formatTime(time.Now())))
}

func rawOf(data any) []byte {
	switch v := data.(type) {
	case nil:
		return []byte("{}")
	case json.RawMessage:
		return v
	case []byte:
		return v
	case string:
		return []byte(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(data)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func randomNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := uint64(0)
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	return strconv.FormatUint(n>>1, 10)
}

//go:embed signature_vectors.json
var vectorsJSON []byte

// SignatureVector 是一个签名的测试向量（与服务端和对外文档相同，sdk/server/testdata/signature_vectors.json）。
type SignatureVector struct {
	Name            string   `json:"name"`
	Secret          string   `json:"secret"`           // 接收端配置的密钥
	SigningSecrets  []string `json:"signing_secrets"`  // 签名用的密钥（没有时为 Secret）
	Timestamp       string   `json:"timestamp"`        // X-IM-Timestamp 的原始字符串
	Nonce           string   `json:"nonce"`            // X-IM-Nonce
	BodyBase64      string   `json:"body_b64"`         // 请求体（原始字节的 base64）
	Expected        string   `json:"expected"`         // 期望的签名（v1=...），没有时为空
	SignatureHeader string   `json:"signature_header"` // 直接使用的 X-IM-Signature（没有时按 SigningSecrets 计算）
	Now             int64    `json:"now"`              // 校验时的当前时间（Unix 秒）
	Valid           bool     `json:"valid"`
	Error           string   `json:"error"`  // 不通过时期望的原因：bad_signature 等
	Reason          string   `json:"reason"` // 说明
}

// Body 返回请求体的原始字节。
func (v SignatureVector) Body() []byte {
	data, _ := base64.StdEncoding.DecodeString(v.BodyBase64)
	return data
}

// Header 返回这个向量的请求头。
func (v SignatureVector) Header() http.Header {
	header := http.Header{}
	header.Set("X-IM-Timestamp", v.Timestamp)
	header.Set("X-IM-Nonce", v.Nonce)
	signature := v.SignatureHeader
	if signature == "" {
		secrets := v.SigningSecrets
		if len(secrets) == 0 {
			secrets = []string{v.Secret}
		}
		signature = callback.SignatureHeader(secrets, v.Timestamp, v.Nonce, v.Body())
	}
	header.Set("X-IM-Signature", signature)
	return header
}

// SignatureVectors 返回签名的测试向量，可以用来确认部署环境（如反向代理没有改动请求体）。
func SignatureVectors() []SignatureVector {
	var out []SignatureVector
	_ = json.Unmarshal(vectorsJSON, &out)
	return out
}
