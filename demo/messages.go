package main

// 发送目标校验、发送结果和消息视图转换。

import (
	"context"
	"encoding/json"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

// targetOf 由会话 ID 推出发送目标：返回 (对端用户, 群 ID)，恰有一个非空。
func (st *AppState) targetOf(ctx *UserContext, conversationID string) (string, string, error) {
	if after, ok := cutPrefix(conversationID, "peer:"); ok {
		if after == "" {
			return "", "", demoErr("invalid_argument", "伪会话缺少用户名")
		}
		return after, "", nil
	}
	st.mu.Lock()
	cst, _ := copyJSON(ctx.Convs[conversationID]).(map[string]any)
	st.mu.Unlock()
	if cst == nil {
		return "", "", demoErr("not_found", "会话不存在或尚未同步: %s", conversationID)
	}
	if strOf(cst["conversation_type"]) == "group" {
		return "", strOf(cst["group_id"]), nil
	}
	return strOf(cst["peer"]), "", nil
}

// send 发一条消息（send_text / send_media 共用）；to_user、to_group 恰好一个非空。
func (st *AppState) send(from, toUser, toGroup, msgType string, body json.RawMessage, ext json.RawMessage, replySeq int64) (*drim.SendResult, error) {
	params := drim.SendMessageParams{
		From:        from,
		Type:        msgType,
		Body:        body,
		Ext:         ext,
		ClientMsgID: newClientMsgID(),
	}
	if toGroup != "" {
		params.ToGroup = toGroup
	} else {
		params.ToUser = toUser
	}
	if replySeq > 0 {
		params.ReplyTo = &drim.ReplyTo{Seq: replySeq}
	}
	return st.client.Messages.Send(context.Background(), params)
}

// afterSend 发送成功后的本地推进与推送；伪会话（peer:）的首条消息重跑会话同步切到真实会话 ID。
func (st *AppState) afterSend(ctx *UserContext, requestedCid string, view map[string]any) (map[string]any, error) {
	cid := strOf(view["conversation_id"])
	if cid == "" {
		cid = requestedCid
	}
	view = messageView(view)
	st.mu.Lock()
	ctx.Msgs[cid] = upsertBySeq(ctx.Msgs[cid], view)
	if cid != requestedCid && hasPrefix(requestedCid, "peer:") {
		st.mu.Unlock()
		st.resyncConvs(ctx)
	} else {
		if cst := ctx.Convs[cid]; cst != nil {
			if seq := int64(num(view["seq"])); seq > int64(num(cst["max_seq"])) {
				cst["max_seq"] = seq
			}
			if at := strOf(view["created_at"]); at != "" {
				cst["last_message_at"] = at
			}
		}
		st.pushConvsLocked(ctx)
		st.mu.Unlock()
	}
	st.mu.Lock()
	st.pushLocked(ctx.Username, "msgs", map[string]any{
		"conversation_id": cid, "messages": []map[string]any{view},
	})
	st.mu.Unlock()
	return view, nil
}

func (st *AppState) sendTarget(username, id string) (string, string, error) {
	if !hasPrefix(id, "peer:") {
		conv, err := st.client.Conversations.StateOfUser(context.Background(), username, id)
		if err != nil {
			return "", "", err
		}
		current := rawMap(conv.Raw)
		if current["conversation_type"] == "group" {
			if current["membership"] != "member" {
				return "", "", demoErr("permission_denied", "已经不在此群中")
			}
			if _, err = st.requireGroup(username, strOf(current["group_id"]), false); err != nil {
				return "", "", err
			}
		}
		st.mu.Lock()
		st.contextOf(username).Convs[id] = current
		st.mu.Unlock()
	}
	return st.targetOf(st.userContext(username), id)
}
