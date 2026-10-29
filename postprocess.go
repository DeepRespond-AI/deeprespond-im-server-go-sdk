package drim

import (
	"bytes"
	"encoding/json"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/endpoints"
)

// beforeDecode 解码之前整理响应（接口描述中的 x-drim-postprocess）。
func beforeDecode(ep *endpoints.Endpoint, body []byte) []byte {
	if ep.Postprocess == "text_check" {
		// 只给 text 时服务端返回单个结果，统一为按字段的结果（键为 text）
		var probe map[string]json.RawMessage
		if json.Unmarshal(body, &probe) == nil {
			if _, ok := probe["fields"]; !ok {
				var buf bytes.Buffer
				buf.WriteString(`{"fields":{"text":`)
				buf.Write(bytes.TrimSpace(body))
				buf.WriteString("}}")
				return buf.Bytes()
			}
		}
	}
	return body
}

// afterDecode 解码之后：按状态码设置标记（x-drim-status）、填入带版本号的列表的版本号，以及个别接口的规则。
func afterDecode(ep *endpoints.Endpoint, out any, status int) {
	if s, ok := out.(interface{ setStatus(int) }); ok {
		s.setStatus(status)
	}
	if v, ok := out.(versionFiller); ok {
		v.fillVersion(ep.VersionField, ep.CountField, ep.MaxCountField)
	}
	switch r := out.(type) {
	case *SendResult:
		if r.OnlineOnly {
			r.Duplicate = false // 只推在线的消息也返回 200，不是重复提交
		}
	case *RoomSendResult:
		switch {
		case r.Dropped:
			r.Duplicate = false // 低优先级的消息被丢弃时也返回 200
		case r.Duplicate:
			// 重复提交时服务端只保证 message_id、created_at 是第一次的，其他字段取自这次请求或为空，置为零值避免误用
			*r = RoomSendResult{
				RoomID: r.RoomID, MessageID: r.MessageID, ClientMsgID: r.ClientMsgID, Priority: r.Priority,
				CreatedAt: r.CreatedAt, Duplicate: true, Raw: r.Raw,
			}
		}
	}
}
