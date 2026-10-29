package main

// 处理好友与黑名单动作。

import (
	"context"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) actionContacts(session *BrowserSession, action string, params map[string]any) (any, error) {
	username := session.Username
	switch action {
	case "friends":
		st.mu.Lock()
		defer st.mu.Unlock()
		return sortedMaps(mapValues(st.contextOf(username).Friends), "username"), nil

	case "requests":
		return st.requestsPayload(st.userContext(username))

	case "lookup_user":
		name := param(params, "username")
		if name == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		u, err := st.client.Users.Get(context.Background(), name)
		if err != nil {
			return nil, err
		}
		return rawMap(u.Raw), nil

	case "add_friend":
		name := param(params, "username")
		if name == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		// 服务端建立好友关系是直接添加（OpenAPI 不提供好友申请的同意/拒绝）
		result := map[string]any{"ok": true}
		_, err := st.client.Friends.Add(context.Background(), username, name, drim.AddFriendParams{})
		if err != nil {
			if !drim.IsCode(err, drim.CodeAlreadyExists) {
				return nil, err
			}
			result["already"] = true
		}
		ctx := st.userContext(username)
		if err := st.refreshFriends(ctx); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushFriendsLocked(ctx)
		st.mu.Unlock()
		st.resyncConvs(ctx)
		return result, nil

	case "accept_friend", "decline_friend":
		return nil, demoErr("unsupported", "服务端 OpenAPI 不提供好友申请的同意/拒绝：需要好友关系时直接添加（friends.add）")

	case "remove_friend":
		name := param(params, "username")
		if _, err := st.client.Friends.Delete(context.Background(), username, name); err != nil {
			return nil, err
		}
		ctx := st.userContext(username)
		if err := st.refreshFriends(ctx); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushFriendsLocked(ctx)
		st.pushConvsLocked(ctx)
		st.mu.Unlock()
		return map[string]any{"ok": true}, nil

	case "remark":
		name, remark := param(params, "username"), param(params, "remark")
		if _, err := st.client.Friends.Update(context.Background(), username, name,
			drim.UpdateFriendParams{Remark: &remark}); err != nil {
			return nil, err
		}
		ctx := st.userContext(username)
		if err := st.refreshFriends(ctx); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushFriendsLocked(ctx)
		st.pushConvsLocked(ctx)
		st.mu.Unlock()
		return map[string]any{"ok": true}, nil

	case "block":
		name := param(params, "username")
		if _, err := st.client.Blacklist.Add(context.Background(), username, name); err != nil {
			return nil, err
		}
		st.pushBlacklist(st.userContext(username))
		return map[string]any{"ok": true}, nil

	case "unblock":
		name := param(params, "username")
		if _, err := st.client.Blacklist.Remove(context.Background(), username, name); err != nil {
			return nil, err
		}
		st.pushBlacklist(st.userContext(username))
		return map[string]any{"ok": true}, nil

	case "blacklist":
		return st.blacklistList(st.userContext(username))
	}

	return nil, demoErr("unknown_action", "未知操作: %s", action)
}
