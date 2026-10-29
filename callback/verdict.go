package callback

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

// 同步回调的结果（《2.接口设计》第 10 节）。返回之前在本地按服务端的规则检查（《1.功能概述文档》第 11.4 节），
// 不合规的不发出，按处理函数出错处理（返回 HookFallback 或 503）并记错误日志。

// MaxResponseBytes 是同步回调响应体的上限（服务端把超过的当作失败）。
const MaxResponseBytes = 64 * 1024

// Verdict 是同步回调的结果，用 Allow、Reject、Modify、Partial 构造；零值不合规（没有返回结果）。
type Verdict struct {
	action   string
	reason   string
	message  string
	body     json.RawMessage
	ext      drim.Field[json.RawMessage]
	rejected []RejectedTarget
}

// RejectedTarget 是部分拒绝中的一个人：Reason、Message 为空时不出现在响应中。
type RejectedTarget struct {
	Username string
	Reason   string
	Message  string
}

// Allow 放行。
func Allow() Verdict { return Verdict{action: "allow"} }

// Reject 拒绝。reason 为 1 到 64 个 [a-z0-9_.-]（客户端得到的 details.app_reason）；message 为给用户看的提示，
// 不超过 256 个字符、不含控制字符（换行、制表符都是）。两者都可以为空，为空时不出现在响应中。
func Reject(reason, message string) Verdict {
	return Verdict{action: "reject", reason: reason, message: message}
}

// Modify 修改消息，只用于 message.before_send、chatroom.before_send：body 为 nil 表示不改（否则是 JSON 对象，整体替换），
// ext 未设置表示不改、Null 表示去掉；至少改一个。修改后的消息不再经过内容审核，由消息模块按正常规则重新校验。
func Modify(body json.RawMessage, ext drim.Field[json.RawMessage]) Verdict {
	return Verdict{action: "modify", body: body, ext: ext}
}

// Partial 部分拒绝，只用于 group.before_join 的邀请和建群（JoinedVia 为 invite、create）：被拒绝的人，至少一项，
// 用户名必须在请求的 Targets 中。
func Partial(rejected ...RejectedTarget) Verdict {
	return Verdict{action: "partial", rejected: rejected}
}

// Action 返回动作：allow、reject、modify、partial；零值为空。
func (v Verdict) Action() string { return v.action }

// VerdictError 是处理函数返回的结果不合规（服务端会按失败处理）。
type VerdictError struct{ Message string }

func (e *VerdictError) Error() string {
	return "callback: 同步回调的结果不合规：" + e.Message
}

func verdictError(format string, args ...any) error {
	return &VerdictError{Message: fmt.Sprintf(format, args...)}
}

var reasonPattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

func checkReason(reason, where string) error {
	if reason != "" && !reasonPattern.MatchString(reason) {
		return verdictError("%sreason 必须是 1 到 64 个小写字母、数字或 _ . -", where)
	}
	return nil
}

func checkMessage(message, where string) error {
	if utf8.RuneCountInString(message) > 256 {
		return verdictError("%smessage 超过 256 个字符", where)
	}
	for _, r := range message {
		if unicode.IsControl(r) {
			return verdictError("%smessage 不能有控制字符（换行、制表符也是）", where)
		}
	}
	if !utf8.ValidString(message) {
		return verdictError("%smessage 不是合法的 UTF-8", where)
	}
	return nil
}

func compactObject(raw json.RawMessage, name string) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, verdictError("%s 必须是 JSON 对象", name)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, verdictError("%s 不是合法的 JSON：%v", name, err)
	}
	return buf.Bytes(), nil
}

// encodeVerdict 检查并编码为响应体；joinedVia、targets 来自 group.before_join 的请求。
func encodeVerdict(v Verdict, hook, joinedVia string, targets []string) ([]byte, error) {
	var out []byte
	switch v.action {
	case "allow":
		out = []byte(`{"action":"allow"}`)
	case "reject":
		if err := checkReason(v.reason, ""); err != nil {
			return nil, err
		}
		if err := checkMessage(v.message, ""); err != nil {
			return nil, err
		}
		out = []byte(`{"action":"reject"`)
		if v.reason != "" {
			out = append(out, `,"reason":`...)
			out = appendJSONString(out, v.reason)
		}
		if v.message != "" {
			out = append(out, `,"message":`...)
			out = appendJSONString(out, v.message)
		}
		out = append(out, '}')
	case "modify":
		if hook != "message.before_send" && hook != "chatroom.before_send" {
			return nil, verdictError("Modify 只能用于 message.before_send、chatroom.before_send，不能用于 %s", hook)
		}
		if v.body == nil && !v.ext.IsSet() {
			return nil, verdictError("Modify 至少要改 body、ext 中的一个")
		}
		out = []byte(`{"action":"modify"`)
		if v.body != nil {
			body, err := compactObject(v.body, "body")
			if err != nil {
				return nil, err
			}
			out = append(out, `,"body":`...)
			out = append(out, body...)
		}
		if v.ext.IsSet() {
			out = append(out, `,"ext":`...)
			if ext, ok := v.ext.Value(); ok {
				compacted, err := compactObject(ext, "ext")
				if err != nil {
					return nil, err
				}
				out = append(out, compacted...)
			} else {
				out = append(out, "null"...)
			}
		}
		out = append(out, '}')
	case "partial":
		if hook != "group.before_join" || (joinedVia != "invite" && joinedVia != "create") {
			return nil, verdictError("Partial 只能用于 group.before_join 的邀请和建群（joined_via 为 invite、create）")
		}
		if len(v.rejected) == 0 {
			return nil, verdictError("Partial 至少要有一项")
		}
		out = []byte(`{"action":"partial","rejected":[`)
		for i, target := range v.rejected {
			where := "第 " + strconv.Itoa(i+1) + " 项的 "
			if !slices.Contains(targets, target.Username) {
				return nil, verdictError("Partial 的第 %d 项 %q 不在请求的 targets 中", i+1, target.Username)
			}
			if err := checkReason(target.Reason, where); err != nil {
				return nil, err
			}
			if err := checkMessage(target.Message, where); err != nil {
				return nil, err
			}
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, `{"username":`...)
			out = appendJSONString(out, target.Username)
			if target.Reason != "" {
				out = append(out, `,"reason":`...)
				out = appendJSONString(out, target.Reason)
			}
			if target.Message != "" {
				out = append(out, `,"message":`...)
				out = appendJSONString(out, target.Message)
			}
			out = append(out, '}')
		}
		out = append(out, "]}"...)
	case "":
		return nil, errors.New("callback: 处理函数没有返回结果（Verdict 的零值），不当作放行")
	default:
		return nil, verdictError("不认识的动作 %q", v.action)
	}
	if len(out) > MaxResponseBytes {
		return nil, verdictError("响应有 %d 字节，超过 64 KB", len(out))
	}
	return out, nil
}

// appendJSONString 写出 JSON 字符串，不转义 HTML 字符。
func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return append(dst, bytes.TrimRight(buf.Bytes(), "\n")...)
}
