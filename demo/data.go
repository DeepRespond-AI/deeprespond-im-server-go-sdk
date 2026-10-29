package main

// SDK 列表读取、用户可见历史和实时群权限。

import (
	"context"
	"log"
	"strconv"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) refreshFriends(ctx *UserContext) error {
	friends := map[string]map[string]any{}
	for f, err := range st.client.Friends.All(context.Background(), ctx.Username, drim.ListFriendsParams{}) {
		if err != nil {
			return err
		}
		m := rawMap(f.Raw)
		friends[strOf(m["username"])] = m
	}
	st.mu.Lock()
	ctx.Friends = friends
	st.mu.Unlock()
	return nil
}

func (st *AppState) refreshGroups(ctx *UserContext) error {
	groups := map[string]map[string]any{}
	for g, err := range st.client.Groups.AllOfUser(context.Background(), ctx.Username, drim.ListOptions{}) {
		if err != nil {
			return err
		}
		m := groupView(rawMap(g.Raw))
		groups[strOf(m["group_id"])] = m
	}
	st.mu.Lock()
	ctx.Groups = groups
	st.mu.Unlock()
	return nil
}

// refreshConvs 重取会话和预览；历史与预览均遵循用户的可见范围。
func (st *AppState) refreshConvs(ctx *UserContext, withLastMessage bool) error {
	convs := map[string]map[string]any{}
	for item, err := range st.client.Conversations.AllOfUser(context.Background(), ctx.Username, drim.ListOptions{}) {
		if err != nil {
			return err
		}
		m := rawMap(item.Raw)
		convs[item.ConversationID] = m
	}
	st.mu.Lock()
	ctx.Convs = make(map[string]map[string]any, len(convs))
	for cid, state := range convs {
		ctx.Convs[cid] = copyJSON(state).(map[string]any)
	}
	for cid, messages := range ctx.Msgs {
		state := convs[cid]
		if state == nil {
			delete(ctx.Msgs, cid)
			continue
		}
		kept := []map[string]any{}
		for _, m := range messages {
			seq := num(m["seq"])
			if seq >= max(1, num(state["start_seq"]), num(state["cleared_seq"])+1) && seq <= num(state["max_seq"]) {
				kept = append(kept, m)
			}
		}
		ctx.Msgs[cid] = kept
	}
	st.mu.Unlock()
	if !withLastMessage {
		return nil
	}
	for cid, state := range convs {
		if state["hidden"] == true {
			continue
		}
		d, err := st.userDevice(ctx.Username)
		if err != nil {
			continue
		}
		page, err := historyPage(d, cid, 0, 1)
		if err != nil {
			log.Printf("[%s] 读取会话预览失败: %v", ctx.Username, errText(err))
			continue
		}
		st.mu.Lock()
		ctx.Msgs[cid] = page["messages"].([]map[string]any)
		st.mu.Unlock()
	}
	return nil
}

func historyPage(d *deviceClient, id string, before int64, limit int) (map[string]any, error) {
	if id == "" {
		return nil, demoErr("invalid_argument", "缺少会话 ID")
	}
	path := conversationPath(id) + "/messages?limit=" + strconv.Itoa(limit)
	if before > 0 {
		path += "&before_seq=" + strconv.FormatInt(before, 10)
	}
	page, err := d.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	if items, ok := page["items"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				messages = append(messages, messageView(m))
			}
		}
	}
	return map[string]any{"conversation_id": id, "messages": messages, "has_more": page["has_more"] == true, "next_before_seq": page["next_before_seq"]}, nil
}

func (st *AppState) visibleMessage(username, id string, seq int64) (map[string]any, error) {
	d, err := st.userDevice(username)
	if err != nil {
		return nil, err
	}
	page, err := d.request("POST", conversationPath(id)+"/messages/lookup", map[string]any{"seqs": []int64{seq}})
	if err != nil {
		return nil, err
	}
	if items, ok := page["items"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && int64(num(m["seq"])) == seq {
				return messageView(m), nil
			}
		}
	}
	return nil, nil
}

func (st *AppState) requireGroup(username, id string, admin bool) (map[string]any, error) {
	if id == "" {
		return nil, demoErr("invalid_argument", "缺少群 ID")
	}
	member, err := st.client.Groups.Member(context.Background(), id, username)
	if err != nil {
		return nil, err
	}
	group, err := st.client.Groups.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if group.Status != "active" || (admin && member.Role != "owner" && member.Role != "admin") {
		return nil, demoErr("permission_denied", "没有执行此群操作的权限")
	}
	view := rawMap(group.Raw)
	view["self"] = rawMap(member.Raw)
	view = groupView(view)
	st.mu.Lock()
	st.contextOf(username).Groups[id] = view
	st.mu.Unlock()
	return view, nil
}

// requestsPayload 好友申请（收到的 + 发出的 + 收到的未读数）。
func (st *AppState) requestsPayload(ctx *UserContext) (map[string]any, error) {
	received, err := st.client.Friends.Requests(context.Background(), ctx.Username,
		drim.ListFriendRequestsParams{Direction: "received", Status: "pending"})
	if err != nil {
		return nil, err
	}
	sent, err := st.client.Friends.Requests(context.Background(), ctx.Username,
		drim.ListFriendRequestsParams{Direction: "sent", Status: "pending"})
	if err != nil {
		return nil, err
	}
	receivedRaw := []map[string]any{}
	unread := int64(0)
	for _, r := range received.Items {
		m := rawMap(r.Raw)
		if m["unread"] == true {
			unread++
		}
		receivedRaw = append(receivedRaw, m)
	}
	sentRaw := []map[string]any{}
	for _, r := range sent.Items {
		sentRaw = append(sentRaw, rawMap(r.Raw))
	}
	return map[string]any{"received": receivedRaw, "sent": sentRaw, "unread": unread}, nil
}

func (st *AppState) blacklistList(ctx *UserContext) ([]string, error) {
	var out []string
	for entry, err := range st.client.Blacklist.All(context.Background(), ctx.Username, drim.ListBlacklistParams{}) {
		if err != nil {
			return nil, err
		}
		out = append(out, entry.Username)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// presenceOf 批量查询在线状态；失败时返回空列表（界面无角标即可，不阻塞流程）。
func (st *AppState) presenceOf(usernames []string) []map[string]any {
	out := []map[string]any{}
	if len(usernames) == 0 {
		return out
	}
	items, err := st.client.Presence.Query(context.Background(), usernames)
	if err != nil {
		log.Printf("查询在线状态失败: %v", errText(err))
		return out
	}
	for _, p := range items {
		out = append(out, presenceView(rawMap(p.Raw)))
	}
	return out
}
