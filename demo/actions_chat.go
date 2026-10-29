package main

// 处理聊天与会话动作。

import (
	"context"
	"encoding/json"
	"strconv"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func (st *AppState) actionChat(session *BrowserSession, action string, params map[string]any) (any, error) {
	username := session.Username
	switch action {
	case "open_conv":
		cid := param(params, "id")
		d, err := st.deviceOf(session)
		if err != nil {
			return nil, err
		}
		page, err := historyPage(d, cid, 0, historyLimit)
		if err != nil {
			return nil, err
		}
		conv, err := st.client.Conversations.StateOfUser(context.Background(), username, cid)
		if err != nil {
			return nil, err
		}
		cst := rawMap(conv.Raw)
		st.mu.Lock()
		ctx := st.contextOf(username)
		ctx.Convs[cid] = cst
		ctx.Msgs[cid] = page["messages"].([]map[string]any)
		page["conversation"] = cst
		if strOf(cst["conversation_type"]) == "group" {
			page["group"] = copyJSON(ctx.Groups[strOf(cst["group_id"])])
		} else {
			page["friend"] = copyJSON(ctx.Friends[strOf(cst["peer"])])
		}
		st.mu.Unlock()
		return page, nil

	case "open_peer":
		peer := param(params, "username")
		conv, err := st.client.Conversations.Single(context.Background(), username, peer)
		if err != nil {
			if drim.IsCode(err, drim.CodeNotFound) {
				return map[string]any{"conversation": nil, "peer": peer}, nil
			}
			return nil, err
		}
		st.mu.Lock()
		st.contextOf(username).Convs[conv.ConversationID] = rawMap(conv.Raw)
		st.pushConvsLocked(st.contextOf(username))
		st.mu.Unlock()
		return st.dispatch(session, "open_conv", map[string]any{"id": conv.ConversationID})

	case "history":
		d, err := st.deviceOf(session)
		if err != nil {
			return nil, err
		}
		return historyPage(d, param(params, "id"), paramInt(params, "before_seq"), 20)

	case "send_text":
		cid, text := param(params, "id"), param(params, "text")
		replySeq := paramInt(params, "reply_seq")
		if cid == "" || text == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		toUser, toGroup, err := st.sendTarget(username, cid)
		if err != nil {
			return nil, err
		}
		if replySeq > 0 {
			d, err := st.deviceOf(session)
			if err != nil {
				return nil, err
			}
			page, err := d.request("POST", conversationPath(cid)+"/messages/lookup", map[string]any{"seqs": []int64{replySeq}})
			if err != nil {
				return nil, err
			}
			if items, ok := page["items"].([]any); !ok || len(items) == 0 {
				return nil, demoErr("permission_denied", "引用的消息不可见")
			}
		}
		body, err := drim.Marshal(map[string]any{"text": text})
		if err != nil {
			return nil, err
		}
		result, err := st.send(username, toUser, toGroup, "text", body, nil, replySeq)
		if err != nil {
			return nil, err
		}
		return st.afterSend(st.userContext(username), cid, rawMap(result.Raw))

	case "send_media":
		cid, msgType := param(params, "id"), param(params, "msg_type")
		fileInfo, _ := params["file"].(map[string]any)
		if cid == "" || (msgType != "image" && msgType != "file") || fileInfo == nil || strOf(fileInfo["file_id"]) == "" {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		toUser, toGroup, err := st.sendTarget(username, cid)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(fileInfo)
		if err != nil {
			return nil, err
		}
		result, err := st.send(username, toUser, toGroup, msgType, body, nil, 0)
		if err != nil {
			return nil, err
		}
		return st.afterSend(st.userContext(username), cid, rawMap(result.Raw))

	case "recall":
		cid, seq := param(params, "id"), paramInt(params, "seq")
		if cid == "" || seq <= 0 {
			return nil, demoErr("invalid_argument", "缺少会话或消息序号")
		}
		d, err := st.deviceOf(session)
		if err != nil {
			return nil, err
		}
		_, err = d.request("POST", conversationPath(cid)+"/messages/"+strconv.FormatInt(seq, 10)+"/recall", nil)
		return map[string]any{"ok": true}, err

	case "delete_msgs":
		cid := param(params, "id")
		seqs := paramIntSlice(params["seqs"])
		if cid == "" || len(seqs) == 0 {
			return nil, demoErr("invalid_argument", "参数不完整")
		}
		_, err := st.client.Conversations.DeleteMessages(context.Background(), username, cid, seqs)
		if err != nil {
			return nil, err
		}
		st.mu.Lock()
		ctx := st.contextOf(username)
		if arr := ctx.Msgs[cid]; arr != nil {
			kept := arr[:0]
			for _, m := range arr {
				drop := false
				for _, s := range seqs {
					if int64(num(m["seq"])) == s {
						drop = true
						break
					}
				}
				if !drop {
					kept = append(kept, m)
				}
			}
			ctx.Msgs[cid] = kept
		}
		st.pushLocked(username, "msgs_deleted", map[string]any{"conversation_id": cid, "seqs": seqs})
		st.mu.Unlock()
		if err := st.refreshConvs(ctx, true); err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.pushConvsLocked(ctx)
		st.mu.Unlock()
		return map[string]any{"ok": true}, nil

	case "react":
		return nil, demoErr("unsupported", "消息回应是客户端长连接专属能力，服务端 OpenAPI 不提供")

	case "read":
		cid, seq := param(params, "id"), paramInt(params, "seq")
		var seqPtr *int64
		if seq > 0 {
			seqPtr = &seq
		}
		change, err := st.client.Conversations.MarkRead(context.Background(), username, cid, seqPtr)
		if err != nil {
			return nil, err
		}
		st.mu.Lock()
		ctx := st.contextOf(username)
		if change.Conversation.ConversationID != "" {
			ctx.Convs[cid] = rawMap(change.Conversation.Raw)
		}
		st.pushConvsLocked(ctx)
		var peer string
		var readSeq int64
		if cst := ctx.Convs[cid]; cst != nil && strOf(cst["conversation_type"]) == "single" {
			peer = strOf(cst["peer"])
			readSeq = int64(num(cst["read_seq"]))
			if readSeq == 0 {
				readSeq = seq
			}
		}
		if peer != "" {
			if peerCtx := st.contexts[peer]; peerCtx != nil && len(peerCtx.Sessions) > 0 {
				st.pushSessionsLocked(sessionsOf(peerCtx), "peer_read", map[string]any{
					"conversation_id": cid, "read_seq": readSeq,
				})
			}
		}
		st.mu.Unlock()
		return map[string]any{"ok": true}, nil

	case "typing":
		// 输入中是客户端长连接能力；demo 在同一进程内转发（多实例部署不可用，见 README）。
		peer := param(params, "toUser")
		if peer == "" {
			peer = param(params, "to_user")
		}
		cid := param(params, "id")
		if peer == "" && cid != "" && !hasPrefix(cid, "peer:") {
			st.mu.Lock()
			if cst := st.contextOf(username).Convs[cid]; cst != nil && strOf(cst["conversation_type"]) == "single" {
				peer = strOf(cst["peer"])
			}
			st.mu.Unlock()
		}
		if peer == "" {
			return map[string]any{"ok": true}, nil
		}
		st.mu.Lock()
		peerCtx := st.contexts[peer]
		var targets []*BrowserSession
		if peerCtx != nil {
			targets = sessionsOf(peerCtx)
		}
		st.mu.Unlock()
		if len(targets) == 0 {
			return map[string]any{"ok": true}, nil
		}
		conv, err := st.client.Conversations.Single(context.Background(), username, peer)
		if err != nil {
			if drim.IsCode(err, drim.CodeNotFound) {
				return map[string]any{"ok": true}, nil
			}
			return nil, err
		}
		realCid := conv.ConversationID

		st.mu.Lock()
		st.pushSessionsLocked(targets, "typing", map[string]any{"conversation_id": realCid, "username": username})
		st.mu.Unlock()
		return map[string]any{"ok": true}, nil

	case "pin_conv":
		return nil, demoErr("unsupported", "会话置顶是客户端专属能力，服务端 OpenAPI 不提供")

	case "delete_conv":
		cid := param(params, "id")
		if _, err := st.client.Conversations.Delete(context.Background(), username, cid, false); err != nil {
			return nil, err
		}
		st.resyncConvs(st.userContext(username))
		return map[string]any{"ok": true}, nil

	case "clear_conv":
		cid := param(params, "id")
		if _, err := st.client.Conversations.Clear(context.Background(), username, cid); err != nil {
			return nil, err
		}
		st.mu.Lock()
		delete(st.contextOf(username).Msgs, cid)
		st.pushLocked(username, "msgs_reset", map[string]any{"conversation_id": cid})
		st.mu.Unlock()
		st.resyncConvs(st.userContext(username))
		return map[string]any{"ok": true}, nil
	}

	return nil, demoErr("unknown_action", "未知操作: %s", action)
}
