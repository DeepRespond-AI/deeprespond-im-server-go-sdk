package main

// 真实设备吊销后再清理浏览器；不依赖回调及时到达。

import (
	"context"
	"time"
)

func (st *AppState) endBrowserSessions(username, reason string, ids map[string]bool) {
	st.mu.Lock()
	ctx := st.contexts[username]
	if ctx == nil {
		st.mu.Unlock()
		return
	}
	var conns []*bridgeConn
	remaining := make([]*BrowserSession, 0, len(ctx.Sessions))
	for _, s := range ctx.Sessions {
		if s.dead || s.closing || (ids != nil && !ids[s.IMSessionID]) {
			remaining = append(remaining, s)
			continue
		}
		s.dead = true
		delete(st.sessions, s.SID)
		if s.ws != nil {
			conns = append(conns, s.ws)
			s.ws = nil
		}
	}
	ctx.Sessions = remaining
	st.mu.Unlock()
	payload := marshalPush("kicked", map[string]any{"reason": reason})
	for _, conn := range conns {
		_ = conn.WriteMessage(payload)
		conn.Close()
	}
}

func (st *AppState) dropSession(s *BrowserSession, closeWS bool) error {
	if s == nil {
		return nil
	}
	st.mu.Lock()
	if s.dead {
		st.mu.Unlock()
		return nil
	}
	s.closing = true
	st.mu.Unlock()
	// 网络失败时保留本地会话，让用户能重试退出。
	err := st.client.Users.Kick(context.Background(), s.Username, s.IMSessionID)
	st.mu.Lock()
	s.closing = false
	if err != nil {
		st.mu.Unlock()
		return err
	}
	s.dead = true
	delete(st.sessions, s.SID)
	if ctx := st.contexts[s.Username]; ctx != nil {
		remaining := make([]*BrowserSession, 0, len(ctx.Sessions))
		for _, item := range ctx.Sessions {
			if item != s {
				remaining = append(remaining, item)
			}
		}
		ctx.Sessions = remaining
	}
	conn := s.ws
	if closeWS {
		s.ws = nil
	}
	st.mu.Unlock()
	if closeWS && conn != nil {
		conn.Close()
	}
	return nil
}

func (st *AppState) kickSelfLater(s *BrowserSession) {
	time.Sleep(1500 * time.Millisecond)
	st.endBrowserSessions(s.Username, "本机已下线", map[string]bool{s.IMSessionID: true})
}

// 回调可能延迟；读取仍有效的设备，避免旧吊销事件误踢新登录。
func (st *AppState) reconcileSessions(username, reason string) error {
	// 先固定候选列表，查询期间创建的新登录不在本次吊销范围。
	st.mu.Lock()
	candidates := map[string]bool{}
	if ctx := st.contexts[username]; ctx != nil {
		for _, s := range ctx.Sessions {
			candidates[s.IMSessionID] = true
		}
	}
	st.mu.Unlock()
	items, err := st.client.Users.Sessions(context.Background(), username)
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	for _, item := range items {
		valid[item.SessionID] = true
	}
	st.mu.Lock()
	invalid := map[string]bool{}
	if ctx := st.contexts[username]; ctx != nil {
		for _, s := range ctx.Sessions {
			if candidates[s.IMSessionID] && !valid[s.IMSessionID] {
				invalid[s.IMSessionID] = true
			}
		}
	}
	st.mu.Unlock()
	st.endBrowserSessions(username, reason, invalid)
	return nil
}
