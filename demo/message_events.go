package main

// 回调只作为变更通知，消息正文重新按用户身份读取权威版本。

import "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"

func (st *AppState) syncChangedMessage(id string, seq int64, kind string) error {
	st.mu.Lock()
	var names []string
	for name, c := range st.contexts {
		if len(c.Sessions) > 0 && c.Convs[id] != nil {
			names = append(names, name)
		}
	}
	st.mu.Unlock()
	for _, name := range names {
		if err := st.syncMessage(name, id, seq, kind); err != nil {
			return err
		}
	}
	return nil
}

func (st *AppState) syncMessage(username, id string, seq int64, kind string) error {
	view, err := st.visibleMessage(username, id, seq)
	if err != nil {
		if e, ok := err.(*drim.Error); ok && (e.StatusCode == 401 || e.StatusCode == 403 || e.StatusCode == 404) {
			return nil
		}
		return err
	}
	if view == nil && kind == "" {
		return nil
	}
	ctx := st.userContext(username)
	if err := st.refreshConvs(ctx, false); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if view == nil {
		kept := []map[string]any{}
		for _, m := range ctx.Msgs[id] {
			if int64(num(m["seq"])) != seq {
				kept = append(kept, m)
			}
		}
		ctx.Msgs[id] = kept
		st.pushLocked(username, "msgs_deleted", map[string]any{"conversation_id": id, "seqs": []int64{seq}})
	} else {
		for _, m := range ctx.Msgs[id] {
			if int64(num(m["seq"])) == seq && num(m["message_version"]) > num(view["message_version"]) {
				view = m
				break
			}
		}
		ctx.Msgs[id] = upsertBySeq(ctx.Msgs[id], view)
		if kind == "" {
			st.pushLocked(username, "msgs", map[string]any{"conversation_id": id, "messages": []map[string]any{view}})
		} else {
			st.pushLocked(username, "msgs_changed", mergeMaps(view, map[string]any{"change": map[string]any{"kind": kind}}))
		}
	}
	st.pushConvsLocked(ctx)
	return nil
}
