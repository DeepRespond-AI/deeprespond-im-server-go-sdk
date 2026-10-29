package callback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 回调请求的签名（《server-go/1.功能概述文档》第 11.2 节，《回调/3.接口设计》第 7.2 节）：签名串为
// "v1:" + X-IM-Timestamp + ":" + X-IM-Nonce + ":" + 请求体，以完整的密钥字符串（含 cbs_ 前缀）为密钥的 HMAC-SHA256 的小写十六进制。
// X-IM-Signature 中可能有多个以逗号分隔的签名（轮换宽限期内为新旧两个），只认 v1=，忽略不认识的版本。

// Tolerance 是时间戳与当前时间相差的上限。
const Tolerance = 5 * time.Minute

// VerifyError 是回调请求的校验没有通过（对方的请求有问题，不是 *drim.Error）。
type VerifyError struct {
	// Reason：bad_signature（签名不对）、bad_timestamp（时间戳不是纯数字）、stale_timestamp（与当前时间相差超过 5 分钟）、
	// bad_format（缺少请求头、随机数格式不对）。
	Reason  string
	Message string
}

func (e *VerifyError) Error() string { return "callback: " + e.Reason + ": " + e.Message }

// 校验失败的原因。
const (
	ReasonBadSignature   = "bad_signature"
	ReasonBadTimestamp   = "bad_timestamp"
	ReasonStaleTimestamp = "stale_timestamp"
	ReasonBadFormat      = "bad_format"
)

// Sign 计算签名（小写十六进制，不含 v1=）。
func Sign(secret, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1:" + timestamp + ":" + nonce + ":"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader 返回 X-IM-Signature 的值：每个密钥一个 v1=，以逗号分隔（新的在前）。
func SignatureHeader(secrets []string, timestamp, nonce string, body []byte) string {
	parts := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		parts = append(parts, "v1="+Sign(secret, timestamp, nonce, body))
	}
	return strings.Join(parts, ",")
}

// Verify 校验签名和时间戳，secrets 为这个地址的新旧密钥；不通过时返回 *VerifyError。
// 签名串中的时间戳用请求头的原始字符串；随机数的去重（防重放）不在这里，由调用方在校验通过后记录。
func Verify(secrets []string, header http.Header, body []byte, now time.Time) error {
	timestamp := headerValue(header, "X-IM-Timestamp")
	nonce := headerValue(header, "X-IM-Nonce")
	signature := headerValue(header, "X-IM-Signature")
	if timestamp == "" || nonce == "" || signature == "" {
		return &VerifyError{ReasonBadFormat, "缺少 X-IM-Timestamp、X-IM-Nonce 或 X-IM-Signature 请求头"}
	}
	if !allDigits(timestamp, 19) {
		return &VerifyError{ReasonBadTimestamp, "X-IM-Timestamp 不是纯数字"}
	}
	if len(nonce) != 32 || !lowerHex(nonce) {
		return &VerifyError{ReasonBadFormat, "X-IM-Nonce 不是 32 个小写十六进制字符"}
	}
	var candidates [][]byte
	for _, item := range strings.Split(signature, ",") {
		item = strings.TrimSpace(item)
		value, ok := strings.CutPrefix(item, "v1=")
		if ok && len(value) == 64 && lowerHex(value) {
			candidates = append(candidates, []byte(value))
		}
	}
	matched := false
	for _, secret := range secrets {
		expected := []byte(Sign(secret, timestamp, nonce, body))
		for _, candidate := range candidates {
			if hmac.Equal(candidate, expected) {
				matched = true
			}
		}
	}
	if !matched {
		return &VerifyError{ReasonBadSignature, "签名不对：密钥配置错误，或请求体被框架、中间件或代理改动过"}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return &VerifyError{ReasonBadTimestamp, "X-IM-Timestamp 超出范围"}
	}
	diff := now.Sub(time.Unix(seconds, 0))
	if diff > Tolerance || diff < -Tolerance {
		return &VerifyError{ReasonStaleTimestamp, "时间戳与当前时间相差超过 5 分钟（检查本机时钟）"}
	}
	return nil
}

func allDigits(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// headerValue 按不区分大小写取请求头（调用方自己构造的 http.Header 的键可能不是规范的写法）。
func headerValue(header http.Header, name string) string {
	if v := header.Get(name); v != "" {
		return v
	}
	for key, values := range header {
		if len(values) > 0 && strings.EqualFold(key, name) {
			return values[0]
		}
	}
	return ""
}
