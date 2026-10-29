package main

// 缓存快照和浏览器推送；网络读取与写连接不占用状态锁。

import (
	"context"
	"encoding/json"
	"log"
	"sort"
)

func marshalPush(event string, data any) []byte {
	b, err := json.Marshal(map[string]any{"type": "push", "event": event, "data": data})
	if err != nil {
		return nil
	}
	return b
}

// pushSessionsLocked 向给定的浏览器会话推送事件（后台发送，不阻塞回调投递）。
func (st *AppState) pushSessionsLocked(sessions []*BrowserSession, event string, data any) {
	payload := marshalPush(event, data)
	for _, s := range sessions {
		if s.ws == nil || payload == nil {
			continue
		}
		conn := s.ws
		go func() {
			_ = conn.WriteMessage(payload) // 写失败由读循环发现并清理
		}()
	}
}

func (st *AppState) pushLocked(username, event string, data any) {
	if ctx := st.contexts[username]; ctx != nil {
		st.pushSessionsLocked(sessionsOf(ctx), event, data)
	}
}

func sessionsOf(ctx *UserContext) []*BrowserSession {
	out := make([]*BrowserSession, len(ctx.Sessions))
	copy(out, ctx.Sessions)
	return out
}

func (st *AppState) pushConvsLocked(ctx *UserContext) {
	st.pushLocked(ctx.Username, "convs", convViews(ctx))
}

// resyncConvs 重跑一次会话状态同步（新群、新好友、伪会话首条消息产生的会话由此落地），
// 完成后推送最新列表。
func (st *AppState) resyncConvs(ctx *UserContext) {
	st.mu.Lock()
	if ctx.Resyncing {
		st.mu.Unlock()
		return
	}
	ctx.Resyncing = true
	st.mu.Unlock()

	defer func() {
		st.mu.Lock()
		ctx.Resyncing = false
		st.mu.Unlock()
	}()
	if err := st.refreshConvs(ctx, true); err != nil {
		log.Printf("[%s] 会话同步失败: %v", ctx.Username, errText(err))
		return
	}
	st.mu.Lock()
	st.pushConvsLocked(ctx)
	st.mu.Unlock()
}

func (st *AppState) pushFriendsLocked(ctx *UserContext) {
	friends := []map[string]any{}
	for _, f := range ctx.Friends {
		friends = append(friends, f)
	}
	sort.Slice(friends, func(i, j int) bool { return strOf(friends[i]["username"]) < strOf(friends[j]["username"]) })
	st.pushLocked(ctx.Username, "friends", friends)
}

func (st *AppState) pushGroupsLocked(ctx *UserContext) {
	groups := []map[string]any{}
	for _, g := range ctx.Groups {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return strOf(groups[i]["group_id"]) < strOf(groups[j]["group_id"]) })
	st.pushLocked(ctx.Username, "groups", groups)
}

func (st *AppState) pushRequests(ctx *UserContext) {
	payload, err := st.requestsPayload(ctx)
	if err != nil {
		log.Printf("[%s] 拉取好友申请失败: %v", ctx.Username, errText(err))
		return
	}
	st.mu.Lock()
	st.pushLocked(ctx.Username, "requests", payload)
	st.mu.Unlock()
}

func (st *AppState) pushBlacklist(ctx *UserContext) {
	list, err := st.blacklistList(ctx)
	if err != nil {
		log.Printf("[%s] 拉取黑名单失败: %v", ctx.Username, errText(err))
		return
	}
	st.mu.Lock()
	st.pushLocked(ctx.Username, "blacklist", list)
	st.mu.Unlock()
}

// bootstrap 登录快照：本人资料 + 各列表数据 + 好友在线状态。
func (st *AppState) bootstrap(session *BrowserSession) {
	if me, err := st.client.Users.Get(context.Background(), session.Username); err == nil {
		st.mu.Lock()
		st.contextOf(session.Username).Me = rawMap(me.Raw)
		st.mu.Unlock()
	} else {
		log.Printf("[%s] 读取本人资料失败: %v", session.Username, errText(err))
	}
	ctx := st.userContext(session.Username)
	_ = st.refreshFriends(ctx)
	_ = st.refreshGroups(ctx)
	_ = st.refreshConvs(ctx, true)
	requests, err := st.requestsPayload(ctx)
	if err != nil {
		log.Printf("[%s] 拉取好友申请失败: %v", ctx.Username, errText(err))
		requests = map[string]any{"received": []any{}, "sent": []any{}, "unread": 0}
	}
	blacklist, err := st.blacklistList(ctx)
	if err != nil {
		blacklist = []string{}
	}

	st.mu.Lock()
	names := make([]string, 0, len(ctx.Friends))
	for name := range ctx.Friends {
		names = append(names, name)
	}
	st.mu.Unlock()
	presence := st.presenceOf(names)
	st.mu.Lock()
	defer st.mu.Unlock()
	friends := []map[string]any{}
	for _, f := range ctx.Friends {
		friends = append(friends, f)
	}
	sort.Slice(friends, func(i, j int) bool { return strOf(friends[i]["username"]) < strOf(friends[j]["username"]) })

	st.pushSessionsLocked([]*BrowserSession{session}, "bootstrap", map[string]any{
		"me":            ctx.Me,
		"self_session":  session.IMSessionID,
		"conversations": convViews(ctx),
		"friends":       friends,
		"requests":      requests,
		"groups":        st.sortedGroupsLocked(ctx),
		"invitations":   []any{},
		"blacklist":     blacklist,
		"presence":      presence,
	})
}

func (st *AppState) sortedGroupsLocked(ctx *UserContext) []map[string]any {
	groups := []map[string]any{}
	for _, g := range ctx.Groups {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return strOf(groups[i]["group_id"]) < strOf(groups[j]["group_id"]) })
	return groups
}
