package main

// 动作入口：验证真实设备身份，按职责分发并返回独立快照。

import (
	"sort"
)

func param(params map[string]any, key string) string {
	if v, ok := params[key]; ok && v != nil {
		return strOf(v)
	}
	return ""
}

func paramInt(params map[string]any, key string) int64 {
	return int64(num(params[key]))
}

func paramList(params map[string]any, key string) []string {
	list, ok := params[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range list {
		if s := strOf(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func mapValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func sortedMaps(items []map[string]any, key string) []map[string]any {
	out := items
	if out == nil {
		out = []map[string]any{}
	}
	sort.Slice(out, func(i, j int) bool { return strOf(out[i][key]) < strOf(out[j][key]) })
	return out
}

func paramIntSlice(v any) []int64 {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []int64
	for _, item := range list {
		out = append(out, int64(num(item)))
	}
	return out
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func cutPrefix(s, prefix string) (string, bool) {
	if hasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func (st *AppState) dispatch(session *BrowserSession, action string, params map[string]any) (data any, err error) {
	device, err := st.deviceOf(session)
	if err != nil {
		return nil, err
	}
	if action != "logout" {
		if _, err = device.request("GET", "/client/v1/me", nil); err != nil {
			return nil, err
		}
	}
	// 不把共享缓存交给锁外的 JSON 序列化，回调可以继续更新原缓存。
	defer func() { st.mu.Lock(); data = copyJSON(data); st.mu.Unlock() }()
	switch action {
	case "open_conv", "open_peer", "history", "send_text", "send_media", "recall", "delete_msgs", "react", "read", "typing", "pin_conv", "delete_conv", "clear_conv":
		return st.actionChat(session, action, params)
	case "friends", "requests", "lookup_user", "add_friend", "accept_friend", "decline_friend", "remove_friend", "remark", "block", "unblock", "blacklist":
		return st.actionContacts(session, action, params)
	case "groups", "create_group", "invite_group", "leave_group", "rename_group", "group_members", "online_count", "invitations", "accept_invitation", "decline_invitation":
		return st.actionGroups(session, action, params)
	case "update_profile", "set_password", "sessions", "revoke_session", "logout":
		return st.actionAccount(session, action, params)
	}
	return nil, demoErr("unknown_action", "未知操作: %s", action)
}
