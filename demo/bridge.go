package main

// 浏览器 WebSocket 接入、动作回复和连接清理。

import (
	"encoding/json"
	"log"
	"net/http"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	session := st.sessionBySID(c.Value)
	if _, err := st.authenticate(session); err != nil {
		jsonError(w, 401, errText(err))
		return
	}
	st.mu.Lock()
	session = st.sessions[c.Value]
	if session == nil || session.dead {
		st.mu.Unlock()
		log.Printf("bridge 拒绝：无会话或已失效")
		// 与客户端 Demo 一致：未登录的桥接直接拒绝
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	st.mu.Unlock()
	conn, err := acceptBridge(w, r)
	if err != nil {
		return
	}
	st.mu.Lock()
	if session.dead || session.closing {
		st.mu.Unlock()
		conn.Close()
		return
	}
	old := session.ws
	session.ws = conn
	st.pushSessionsLocked([]*BrowserSession{session}, "conn", map[string]any{"state": "connected"})
	st.mu.Unlock()
	if old != nil {
		old.Close()
	}
	defer func() {
		conn.Close()
		st.mu.Lock()
		if session.ws == conn {
			session.ws = nil
		}
		st.mu.Unlock()
	}()
	log.Printf("bridge 已连接: user=%s session=%s", session.Username, session.IMSessionID)

	st.bootstrap(session)

	for {
		payload, err := conn.ReadMessage()
		if err != nil {
			st.mu.Lock()
			if session.ws == conn {
				session.ws = nil
			}
			st.mu.Unlock()
			log.Printf("bridge 断开: user=%s session=%s err=%v", session.Username, session.IMSessionID, err)
			return
		}
		var req struct {
			ID     any            `json:"id"`
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		st.mu.Lock()
		dead := session.dead
		st.mu.Unlock()
		if dead {
			st.reply(conn, req.ID, nil, "会话已失效，请重新登录")
			continue
		}
		data, err := st.dispatch(session, req.Action, req.Params)
		if err != nil {
			st.reply(conn, req.ID, nil, errText(err))
			if e, ok := err.(*drim.Error); ok && e.StatusCode == 401 {
				st.endBrowserSessions(session.Username, "登录状态已失效", map[string]bool{session.IMSessionID: true})
				return
			}
			continue
		}
		st.reply(conn, req.ID, data, "")
		st.mu.Lock()
		dead = session.dead
		st.mu.Unlock()
		if dead {
			return
		}
	}
}

func (st *AppState) reply(conn *bridgeConn, id any, data any, errMsg string) {
	payload := map[string]any{"type": "reply", "id": id, "ok": errMsg == ""}
	if errMsg != "" {
		payload["error"] = errMsg
	} else {
		payload["data"] = data
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = conn.WriteMessage(b)
}
