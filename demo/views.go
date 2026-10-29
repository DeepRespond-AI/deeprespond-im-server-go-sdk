package main

// 纯视图转换和 JSON 快照，不访问网络。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

const (
	historyLimit = 30
	cacheLimit   = 200
)

// errText 把错误变成给界面的一句话（code 保留，便于排错）。
func errText(err error) string {
	var e *drim.Error
	if asDrimError(err, &e) {
		msg := e.Message
		if msg == "" {
			msg = e.Code
		}
		return fmt.Sprintf("%s（code=%s）", msg, e.Code)
	}
	return err.Error()
}

func demoErr(code, format string, args ...any) error {
	return &drim.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func asDrimError(err error, target **drim.Error) bool {
	e, ok := err.(*drim.Error)
	if ok {
		*target = e
	}
	return ok
}

func newClientMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// rawMap 把服务端返回的对象（有 Raw 的类型）变成视图用的 map；解析失败时返回最小可用的形状。
func rawMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// num 取 map 里的数值：REST 原始 JSON 解出来是 float64，回调事件字段是 int64，都要支持。
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// strOf 取 map 里的字符串，缺省空串。
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// formatTime 统一成 RFC3339（浏览器 new Date 可解析）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func upsertBySeq(items []map[string]any, item map[string]any) []map[string]any {
	seq := num(item["seq"])
	for i, existing := range items {
		if num(existing["seq"]) == seq {
			items[i] = item
			return items
		}
	}
	items = append(items, item)
	sort.SliceStable(items, func(i, j int) bool { return num(items[i]["seq"]) < num(items[j]["seq"]) })
	if len(items) > cacheLimit {
		items = items[len(items)-cacheLimit:]
	}
	return items
}

func previewOf(m map[string]any) string {
	if m["recalled"] != nil {
		return "撤回了一条消息"
	}
	mtype := strOf(m["type"])
	text := ""
	if body, ok := m["body"].(map[string]any); ok {
		text = strOf(body["text"])
	}
	switch mtype {
	case "text":
		return text
	case "image":
		return "[图片]"
	case "file":
		return "[文件]"
	case "voice":
		return "[语音]"
	case "video":
		return "[视频]"
	case "tip":
		if text != "" {
			return text
		}
		return "[提示]"
	default:
		return "[自定义消息]"
	}
}

// convViews 组装会话列表视图：置顶在前，其余按最后消息时间倒序。
// 必须在持锁状态下调用（读 ctx.convs / ctx.msgs / ctx.groups / ctx.friends）。
func convViews(ctx *UserContext) []map[string]any {
	out := []map[string]any{}
	for _, st := range ctx.Convs {
		if st["hidden"] == true {
			continue // 已删除的会话不进列表（清空历史的会话仍显示）
		}
		peer := ""
		if st["peer"] != nil {
			peer = strOf(st["peer"])
		}
		groupID := ""
		if st["group_id"] != nil {
			groupID = strOf(st["group_id"])
		}
		lastAt := ""
		if st["last_message_at"] != nil {
			lastAt = strOf(st["last_message_at"])
		}
		view := map[string]any{
			"id":            strOf(st["conversation_id"]),
			"type":          strOf(st["conversation_type"]),
			"peer":          peerOrNil(peer),
			"group_id":      groupIDOrNil(groupID),
			"name":          "",
			"unread":        int64(num(st["unread_count"])),
			"pinned":        st["pinned_at"] != nil,
			"marked_unread": st["marked_unread"] == true,
			"hidden":        st["hidden"] == true,
			"max_seq":       int64(num(st["max_seq"])),
			"last_seq":      int64(num(st["max_seq"])),
			"last_preview":  "",
			"last_at":       lastAt,
			"last_sender":   "",
			"peer_read_seq": st["peer_read_seq"],
			"membership":    st["membership"],
		}
		if strOf(st["conversation_type"]) == "group" {
			view["name"] = "群 " + groupID
			if g := ctx.Groups[groupID]; g != nil && strOf(g["name"]) != "" {
				view["name"] = strOf(g["name"])
			}
		} else {
			view["name"] = peer
			if f := ctx.Friends[peer]; f != nil {
				if remark := strOf(f["remark"]); remark != "" {
					view["name"] = remark
				} else if nickname := strOf(f["nickname"]); nickname != "" {
					view["name"] = nickname
				}
			}
		}
		if cached := ctx.Msgs[view["id"].(string)]; len(cached) > 0 {
			m := cached[len(cached)-1]
			view["last_seq"] = int64(num(m["seq"]))
			if s := strOf(m["created_at"]); s != "" {
				view["last_at"] = s
			}
			view["last_preview"] = previewOf(m)
			switch {
			case strOf(m["sender"]) == ctx.Username:
				view["last_sender"] = "self"
			case strOf(m["sender_type"]) == "system":
				view["last_sender"] = "system"
			default:
				view["last_sender"] = strOf(m["sender"])
			}
		}
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool { return strOf(out[i]["last_at"]) > strOf(out[j]["last_at"]) })
	sort.SliceStable(out, func(i, j int) bool { return out[i]["pinned"] == true && out[j]["pinned"] != true })
	return out
}

func peerOrNil(peer string) any {
	if peer == "" {
		return nil
	}
	return peer
}

func groupIDOrNil(gid string) any {
	if gid == "" {
		return nil
	}
	return gid
}

// groupView 把群的原始 JSON 扁平化成界面用的 GroupInfo 形状（self.role → my_role）。
func groupView(raw map[string]any) map[string]any {
	view := map[string]any{}
	for k, v := range raw {
		view[k] = v
	}
	myRole := "member"
	if self, ok := raw["self"].(map[string]any); ok {
		if role := strOf(self["role"]); role != "" {
			myRole = role
		}
	}
	view["my_role"] = myRole
	delete(view, "self")
	return view
}

func presenceView(raw map[string]any) map[string]any {
	online := raw["online"] == true
	platforms := []string{}
	if list, ok := raw["platforms"].([]any); ok {
		for _, p := range list {
			if s := strOf(p); s != "" {
				platforms = append(platforms, s)
			}
		}
	}
	lastSeen := ""
	if s := strOf(raw["last_seen_at"]); s != "" {
		lastSeen = s
	}
	return map[string]any{
		"username":     strOf(raw["username"]),
		"online":       online,
		"state":        map[bool]string{true: "online", false: "offline"}[online],
		"platforms":    platforms,
		"last_seen_at": lastSeen,
		"version":      int64(num(raw["version"])),
	}
}

// copyJSON 在持锁时复制视图，避免缓存与回复共享可变的 map、切片。
func copyJSON(v any) any {
	switch value := v.(type) {
	case map[string]any:
		if value == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = copyJSON(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(value))
		for i, item := range value {
			out[i] = copyJSON(item).(map[string]any)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = copyJSON(item)
		}
		return out
	case json.RawMessage:
		return append(json.RawMessage(nil), value...)
	case []string:
		return append([]string{}, value...)
	default:
		return v
	}
}

func messageView(m map[string]any) map[string]any {
	if m["sender_type"] == nil {
		m["sender_type"] = "user"
	}
	if m["sender"] == nil {
		m["sender"] = m["sender_username"]
	}
	if m["recipient"] == nil {
		m["recipient"] = m["recipient_username"]
	}
	return m
}
