package main

// 处理群组动作。

import (
	"context"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) actionGroups(session *BrowserSession, action string, params map[string]any) (any, error) {
	username := session.Username
	switch action {
	case "groups":
		st.mu.Lock()
		defer st.mu.Unlock()
		return sortedMaps(mapValues(st.contextOf(username).Groups), "group_id"), nil

	case "create_group":
		name := param(params, "name")
		var members []string
		for _, m := range paramList(params, "members") {
			if m != "" && m != username {
				members = append(members, m)
			}
		}
		if name == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		g, err := st.client.Groups.Create(context.Background(), drim.CreateGroupParams{
			Owner: username, Name: name, Members: members,
		})
		if err != nil {
			return nil, err
		}
		ctx := st.userContext(username)
		st.mu.Lock()
		ctx.Groups[strOf(rawMap(g.Raw)["group_id"])] = groupView(rawMap(g.Raw))
		st.pushGroupsLocked(ctx)
		st.mu.Unlock()
		st.resyncConvs(ctx)
		return rawMap(g.Raw), nil

	case "invite_group":
		gid, users := param(params, "group_id"), paramList(params, "users")
		if gid == "" || len(users) == 0 {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		group, err := st.requireGroup(username, gid, false)
		if err != nil {
			return nil, err
		}
		role := strOf(group["my_role"])
		if role != "owner" && role != "admin" && group["member_invite"] != "free" {
			return nil, demoErr("permission_denied", "当前成员不能直接邀请入群")
		}
		res, err := st.client.Groups.AddMembers(context.Background(), gid, users)
		if err != nil {
			return nil, err
		}
		ctx := st.userContext(username)
		if err := st.refreshGroups(ctx); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushGroupsLocked(ctx)
		st.mu.Unlock()
		return rawMap(res.Raw), nil

	case "leave_group":
		gid := param(params, "group_id")
		group, err := st.requireGroup(username, gid, false)
		if err != nil {
			return nil, err
		}
		if group["my_role"] == "owner" {
			return nil, demoErr("permission_denied", "群主需要先转让群主才能退出")
		}
		if _, err := st.client.Groups.RemoveMembers(context.Background(), gid, []string{username}); err != nil {
			return nil, err
		}
		ctx := st.userContext(username)
		if err := st.refreshGroups(ctx); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushGroupsLocked(ctx)
		st.mu.Unlock()
		st.resyncConvs(ctx)
		return map[string]any{"ok": true}, nil

	case "rename_group":
		gid, name := param(params, "id"), param(params, "name")
		if gid == "" || name == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		group, err := st.requireGroup(username, gid, true)
		if err != nil {
			return nil, err
		}
		change, err := st.client.Groups.Update(context.Background(), gid, drim.UpdateGroupParams{Name: &name})
		if err != nil {
			return nil, err
		}
		view := mergeMaps(group, rawMap(change.Raw))
		ctx := st.userContext(username)
		st.mu.Lock()
		ctx.Groups[gid] = view
		st.pushGroupsLocked(ctx)
		st.pushConvsLocked(ctx)
		st.mu.Unlock()
		return view, nil

	case "group_members":
		gid := param(params, "group_id")
		if _, err := st.requireGroup(username, gid, false); err != nil {
			return nil, err
		}
		members := []map[string]any{}
		for m, err := range st.client.Groups.AllMembers(context.Background(), gid, drim.ListMembersParams{}) {
			if err != nil {
				return nil, err
			}
			members = append(members, rawMap(m.Raw))
		}
		return members, nil

	case "online_count":
		gid := param(params, "group_id")
		if _, err := st.requireGroup(username, gid, false); err != nil {
			return nil, err
		}
		var usernames []string
		for m, err := range st.client.Groups.AllMembers(context.Background(), gid, drim.ListMembersParams{}) {
			if err != nil {
				return nil, err
			}
			usernames = append(usernames, m.Username)
		}
		if len(usernames) > 100 {
			usernames = usernames[:100]
		}
		items := st.presenceOf(usernames)
		online := int64(0)
		for _, p := range items {
			if p["online"] == true {
				online++
			}
		}
		return map[string]any{"online_count": online}, nil

	case "invitations":
		// 群邀请由客户端处理，服务端直接加人；OpenAPI 没有面向用户的邀请列表
		return []any{}, nil

	case "accept_invitation", "decline_invitation":
		return nil, demoErr("unsupported", "群邀请由客户端处理，服务端直接加人（groups.add_members），OpenAPI 不提供接受/拒绝")
	}

	return nil, demoErr("unknown_action", "未知操作: %s", action)
}
