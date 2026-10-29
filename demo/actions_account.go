package main

// 处理账号与设备动作。

import (
	"context"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) actionAccount(session *BrowserSession, action string, params map[string]any) (any, error) {
	username := session.Username
	switch action {
	case "update_profile":
		nickname := param(params, "nickname")
		if nickname == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		if _, err := st.client.Users.Update(context.Background(), username,
			drim.UpdateUserParams{Nickname: &nickname}); err != nil {
			return nil, err
		}
		me, err := st.client.Users.Get(context.Background(), username)
		if err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.contextOf(username).Me = rawMap(me.Raw)
		st.pushLocked(username, "profile", st.contextOf(username).Me)
		st.mu.Unlock()
		return rawMap(me.Raw), nil

	case "set_password":
		oldPwd, newPwd := param(params, "old"), param(params, "new")
		if oldPwd == "" || newPwd == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		if len(newPwd) < 6 {
			return nil, demoErr("invalid_argument", "新密码至少 6 位")
		}
		d, err := st.deviceOf(session)
		if err != nil {
			return nil, err
		}
		_, err = d.request("PUT", "/client/v1/me/password", map[string]any{"current_password": oldPwd, "new_password": newPwd})
		return map[string]any{"ok": true}, err

	case "sessions":
		items, err := st.client.Users.Sessions(context.Background(), username)
		if err != nil {
			return nil, err
		}
		raw := []map[string]any{}
		for _, s := range items {
			raw = append(raw, rawMap(s.Raw))
		}
		st.mu.Lock()
		current := session.IMSessionID
		st.mu.Unlock()
		return map[string]any{"items": raw, "current": current}, nil

	case "revoke_session":
		sid := param(params, "session_id")
		if sid == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		if err := st.client.Users.Kick(context.Background(), username, sid); err != nil {
			return nil, err
		}
		st.mu.Lock()
		self := sid == session.IMSessionID
		st.mu.Unlock()
		if self {
			// 兜底：回调偶发丢失时也结束本机的浏览器会话
			go st.kickSelfLater(session)
		}
		return map[string]any{"ok": true, "self": self}, nil

	case "logout":
		err := st.dropSession(session, false)
		return map[string]any{"ok": true}, err
	}

	return nil, demoErr("unknown_action", "未知操作: %s", action)
}
