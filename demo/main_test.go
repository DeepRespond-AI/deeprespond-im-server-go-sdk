package main

import (
	"encoding/json"
	"testing"
)

func TestPreviewOf(t *testing.T) {
	cases := []struct {
		msg  map[string]any
		want string
	}{
		{map[string]any{"type": "text", "body": map[string]any{"text": "你好"}}, "你好"},
		{map[string]any{"type": "image", "body": nil}, "[图片]"},
		{map[string]any{"type": "file", "body": nil}, "[文件]"},
		{map[string]any{"type": "voice", "body": nil}, "[语音]"},
		{map[string]any{"type": "video", "body": nil}, "[视频]"},
		{map[string]any{"type": "custom", "body": nil}, "[自定义消息]"},
		{map[string]any{"type": "tip", "body": map[string]any{"text": ""}}, "[提示]"},
		{map[string]any{"type": "text", "recalled": map[string]any{"by": "alice"}}, "撤回了一条消息"},
	}
	for _, c := range cases {
		if got := previewOf(c.msg); got != c.want {
			t.Errorf("previewOf(%v) = %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestUpsertBySeqOrdersAndTrims(t *testing.T) {
	var items []map[string]any
	mk := func(seq int64) map[string]any { return map[string]any{"seq": float64(seq)} }
	for _, s := range []int64{3, 1, 2} {
		items = upsertBySeq(items, mk(s))
	}
	if len(items) != 3 || num(items[0]["seq"]) != 1 || num(items[2]["seq"]) != 3 {
		t.Fatalf("乱序插入后应按 seq 升序: %v", items)
	}
	items = upsertBySeq(items, map[string]any{"seq": float64(2), "text": "updated"})
	if len(items) != 3 || items[1]["text"] != "updated" {
		t.Fatalf("同 seq 应覆盖而不是追加: %v", items)
	}
	for s := int64(4); s < cacheLimit+10; s++ {
		items = upsertBySeq(items, mk(s))
	}
	if len(items) != cacheLimit {
		t.Fatalf("缓存应裁剪到 %d 条: %d", cacheLimit, len(items))
	}
	// seqs 1..3 加 4..(cacheLimit+9)，裁剪后保留最新的 cacheLimit 条（10..209）
	if min := num(items[0]["seq"]); min != float64(cacheLimit+9-cacheLimit+1) {
		t.Fatalf("裁剪应保留最新的消息: 最小 seq=%v", items[0]["seq"])
	}
}

func newTestState() *AppState {
	st := newState(nil, "http://127.0.0.1:18480", "6526900072#myapp", nil)
	return st
}

func TestConvViewsOrderAndFields(t *testing.T) {
	st := newTestState()
	ctx := st.contextOf("alice")
	// 故意乱序插入：群会话（最新）置顶前普通会话按时间倒序，置顶最前
	ctx.Convs["c-group"] = map[string]any{
		"conversation_id": "c-group", "conversation_type": "group", "group_id": "g1",
		"max_seq": float64(5), "unread_count": float64(2), "last_message_at": "2026-10-21T10:00:00Z",
	}
	ctx.Convs["c-old"] = map[string]any{
		"conversation_id": "c-old", "conversation_type": "single", "peer": "bob",
		"max_seq": float64(1), "unread_count": float64(0), "last_message_at": "2026-10-21T08:00:00Z",
	}
	ctx.Convs["c-pinned"] = map[string]any{
		"conversation_id": "c-pinned", "conversation_type": "single", "peer": "carol",
		"max_seq": float64(2), "unread_count": float64(0), "pinned_at": "2026-10-20T00:00:00Z",
		"last_message_at": "2026-10-21T07:00:00Z",
	}
	ctx.Groups["g1"] = groupView(map[string]any{"group_id": "g1", "name": "项目群"})
	ctx.Friends["bob"] = map[string]any{"username": "bob", "remark": "老王", "nickname": "bob"}
	ctx.Friends["carol"] = map[string]any{"username": "carol"}

	st.mu.Lock()
	defer st.mu.Unlock()
	views := convViews(ctx)
	if len(views) != 3 {
		t.Fatalf("应有 3 个会话: %d", len(views))
	}
	if views[0]["id"] != "c-pinned" {
		t.Fatalf("置顶会话应排最前: %v", views[0]["id"])
	}
	if views[1]["id"] != "c-group" || views[2]["id"] != "c-old" {
		t.Fatalf("其余按最后消息时间倒序: %v, %v", views[1]["id"], views[2]["id"])
	}
	if views[1]["name"] != "项目群" || views[1]["unread"] != int64(2) {
		t.Fatalf("群会话视图: %v", views[1])
	}
	if views[2]["name"] != "老王" {
		t.Fatalf("单聊应优先用好友备注: %v", views[2]["name"])
	}
}

func TestConvViewsHidesDeleted(t *testing.T) {
	st := newTestState()
	ctx := st.contextOf("alice")
	ctx.Convs["c-hidden"] = map[string]any{
		"conversation_id": "c-hidden", "conversation_type": "single", "peer": "bob", "hidden": true,
	}
	st.mu.Lock()
	views := convViews(ctx)
	st.mu.Unlock()
	if len(views) != 0 {
		t.Fatalf("已删除的会话不应出现在列表: %v", views)
	}
}

func TestGroupViewFlattensSelf(t *testing.T) {
	view := groupView(map[string]any{
		"group_id": "g1", "name": "群",
		"self": map[string]any{"role": "owner", "group_nickname": ""},
	})
	if view["my_role"] != "owner" {
		t.Fatalf("my_role 应来自 self.role: %v", view["my_role"])
	}
	if _, ok := view["self"]; ok {
		t.Fatalf("self 应被移除: %v", view)
	}
	if v := groupView(map[string]any{"group_id": "g2"})["my_role"]; v != "member" {
		t.Fatalf("缺 self 时 my_role 缺省 member: %v", v)
	}
}

func TestPresenceView(t *testing.T) {
	view := presenceView(map[string]any{
		"username": "bob", "online": true, "platforms": []any{"web", "ios"}, "version": float64(3),
	})
	if view["state"] != "online" || view["online"] != true {
		t.Fatalf("在线视图: %v", view)
	}
	platforms, _ := view["platforms"].([]string)
	if len(platforms) != 2 {
		t.Fatalf("platforms: %v", view["platforms"])
	}
	offline := presenceView(map[string]any{"username": "bob", "online": false})
	if offline["state"] != "offline" {
		t.Fatalf("离线视图: %v", offline)
	}
}

func TestParseCallbackCredentials(t *testing.T) {
	got := parseCallbackCredentials(" ep1:s1 , ep2:s2a:s2b,")
	if len(got) != 2 || got["ep1"][0] != "s1" || got["ep2"][0] != "s2a:s2b" {
		t.Fatalf("解析回调凭据: %v", got)
	}
	if len(parseCallbackCredentials("")) != 0 {
		t.Fatal("空串应为空表")
	}
}

func TestTargetOf(t *testing.T) {
	st := newTestState()
	ctx := st.contextOf("alice")
	ctx.Convs["c1"] = map[string]any{"conversation_id": "c1", "conversation_type": "single", "peer": "bob"}
	ctx.Convs["c2"] = map[string]any{"conversation_id": "c2", "conversation_type": "group", "group_id": "g1"}

	toUser, toGroup, err := st.targetOf(ctx, "c1")
	if err != nil || toUser != "bob" || toGroup != "" {
		t.Fatalf("单聊目标: %v %v %v", toUser, toGroup, err)
	}
	toUser, toGroup, err = st.targetOf(ctx, "c2")
	if err != nil || toUser != "" || toGroup != "g1" {
		t.Fatalf("群聊目标: %v %v %v", toUser, toGroup, err)
	}
	toUser, toGroup, err = st.targetOf(ctx, "peer:carol")
	if err != nil || toUser != "carol" || toGroup != "" {
		t.Fatalf("伪会话目标: %v %v %v", toUser, toGroup, err)
	}
	if _, _, err := st.targetOf(ctx, "peer:"); err == nil {
		t.Fatal("空伪会话应报错")
	}
	if _, _, err := st.targetOf(ctx, "c-none"); err == nil {
		t.Fatal("未知会话应报错")
	}
}

func TestJSONViewRoundTrip(t *testing.T) {
	// 视图中的消息 raw JSON 应解析为 JSON 对象形状（与 Python 版 dict 视图一致）
	view := rawMap(json.RawMessage(`{"text":"hi","n":1,"body":{"text":"inner"}}`))
	if view["text"] != "hi" || view["n"] != float64(1) {
		t.Fatalf("rawMap 解析: %v", view)
	}
	body, ok := view["body"].(map[string]any)
	if !ok || body["text"] != "inner" {
		t.Fatalf("嵌套 body: %v", view["body"])
	}
}
