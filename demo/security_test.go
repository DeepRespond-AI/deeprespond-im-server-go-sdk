package main

// 回归权限、消息可见性、设备吊销和共享缓存；客户端与管理接口分别记录。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

type deviceReply struct {
	status int
	body   any
}
type fixture struct {
	st      *AppState
	sdk     *drimtest.Server
	session *BrowserSession
	mu      sync.Mutex
	replies map[string]deviceReply
	calls   []string
}

func setupDemo(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{sdk: drimtest.NewServer(), replies: map[string]deviceReply{}}
	t.Cleanup(f.sdk.Close)
	client, err := drim.New(f.sdk.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		key := r.Method + " " + r.URL.RequestURI()
		f.calls = append(f.calls, key)
		result, ok := f.replies[key]
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Errorf("用户请求没有使用设备令牌: %s", key)
		}
		if !ok {
			if key == "GET /client/v1/me" {
				result = deviceReply{200, map[string]any{"username": "alice"}}
			} else {
				result = deviceReply{403, map[string]any{"code": "permission_denied", "message": "没有权限"}}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.status)
		_ = json.NewEncoder(w).Encode(result.body)
	}))
	t.Cleanup(device.Close)
	f.st = newState(client, device.URL, f.sdk.OrgName+"#"+f.sdk.AppName, nil)
	f.session = &BrowserSession{SID: "sid", Username: "alice", IMSessionID: "real-session", device: newDevice(device.URL, map[string]any{"access_token": "user-token"})}
	f.st.sessions[f.session.SID] = f.session
	f.st.contextOf("alice").Sessions = []*BrowserSession{f.session}
	return f
}

func (f *fixture) respond(path string, status int, body any) {
	f.mu.Lock()
	f.replies[path] = deviceReply{status, body}
	f.mu.Unlock()
}

func TestBrowserActionsDoNotBorrowAdminMessagePermissions(t *testing.T) {
	for _, action := range []string{"history", "recall", "open_conv"} {
		t.Run(action, func(t *testing.T) {
			f := setupDemo(t)
			_, err := f.st.dispatch(f.session, action, map[string]any{"id": "victim", "seq": float64(1)})
			if err == nil {
				t.Fatal("越权动作应被拒绝")
			}
			if len(f.sdk.Calls("conversations.messages")) > 0 || len(f.sdk.Calls("messages.recall")) > 0 {
				t.Fatal("用户消息操作不应使用管理员接口")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.calls) != 2 {
				t.Fatalf("应验证设备并请求用户接口: %v", f.calls)
			}
		})
	}
}

func TestExpiredDeviceCannotExecuteAdminAction(t *testing.T) {
	f := setupDemo(t)
	f.respond("GET /client/v1/me", 401, map[string]any{"code": "unauthenticated"})
	_, err := f.st.dispatch(f.session, "update_profile", map[string]any{"nickname": "not allowed"})
	if err == nil || len(f.sdk.Requests()) != 0 {
		t.Fatal("设备失效后仍调用了管理接口")
	}
}

func TestGroupPermissionsUseCurrentMembership(t *testing.T) {
	for _, role := range []string{"outsider", "member", "admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			f := setupDemo(t)
			f.sdk.Respond("groups.get", 200, map[string]any{"group_id": "g", "status": "active", "member_invite": "admin"})
			if role == "outsider" {
				f.sdk.Respond("groups.member", 404, map[string]any{"code": "not_found"})
			} else {
				f.sdk.Respond("groups.member", 200, map[string]any{"username": "alice", "role": role})
			}
			// 过期的 owner 缓存不能放大权限。
			f.st.contextOf("alice").Groups["g"] = map[string]any{"my_role": "owner"}
			_, err := f.st.dispatch(f.session, "rename_group", map[string]any{"id": "g", "name": "new"})
			allowed := role == "admin" || role == "owner"
			if (err == nil) != allowed {
				t.Fatalf("role=%s err=%v", role, err)
			}
			if !allowed && len(f.sdk.Calls("groups.update")) != 0 {
				t.Fatal("无权限时不应修改群")
			}
		})
	}
}

func TestPreviewDoesNotReplaceHistoryAndCursor(t *testing.T) {
	f := setupDemo(t)
	ctx := f.st.contextOf("alice")
	ctx.Convs["c"] = map[string]any{"conversation_id": "c"}
	ctx.Msgs["c"] = []map[string]any{{"seq": 30}}
	items := []any{}
	for i := 1; i <= 30; i++ {
		items = append(items, map[string]any{"seq": i})
	}
	f.respond("GET /client/v1/conversations/c/messages?limit=30", 200, map[string]any{"items": items, "has_more": true, "next_before_seq": 7})
	out, err := f.st.dispatch(f.session, "open_conv", map[string]any{"id": "c"})
	if err != nil {
		t.Fatal(err)
	}
	page := out.(map[string]any)
	if len(page["messages"].([]map[string]any)) != 30 || page["has_more"] != true || num(page["next_before_seq"]) != 7 {
		t.Fatal(page)
	}
	f.respond("GET /client/v1/conversations/c/messages?limit=20&before_seq=7", 200, map[string]any{"items": []any{}, "has_more": true, "next_before_seq": 2})
	out, err = f.st.dispatch(f.session, "history", map[string]any{"id": "c", "before_seq": 7})
	if err != nil || out.(map[string]any)["has_more"] != true {
		t.Fatal(out, err)
	}
}

func TestDeletedMessageIsNotRestoredByCallback(t *testing.T) {
	f := setupDemo(t)
	ctx := f.st.contextOf("alice")
	ctx.Convs["c"] = map[string]any{"conversation_id": "c"}
	f.respond("POST /client/v1/conversations/c/messages/lookup", 200, map[string]any{"items": []any{}})
	if err := f.st.syncMessage("alice", "c", 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(ctx.Msgs["c"]) != 0 {
		t.Fatal("重试回调恢复了已删除的消息")
	}
}

func TestEditedCallbackReadsAuthoritativeBody(t *testing.T) {
	f := setupDemo(t)
	ctx := f.st.contextOf("alice")
	ctx.Convs["c"] = map[string]any{"conversation_id": "c"}
	ctx.Msgs["c"] = []map[string]any{{"seq": float64(1), "body": map[string]any{"text": "old"}}}
	f.sdk.Respond("conversations.of_user", 200, `{"items":[{"conversation_id":"c","max_seq":1}]}`)
	f.respond("POST /client/v1/conversations/c/messages/lookup", 200, map[string]any{"items": []any{map[string]any{"conversation_id": "c", "seq": 1, "body": map[string]any{"text": "new"}, "message_version": 2}}})
	if err := f.st.eventHandlers().MessageEdited(context.Background(), nil, &callback.MessageEdited{ConversationID: "c", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if ctx.Msgs["c"][0]["body"].(map[string]any)["text"] != "new" {
		t.Fatal(ctx.Msgs)
	}
}

func TestLogoutRevokesDeviceAndPreservesSessionOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "成功", true: "失败可重试"}[fail], func(t *testing.T) {
			f := setupDemo(t)
			if fail {
				f.sdk.Respond("users.kick", 403, map[string]any{"code": "permission_denied"})
			}
			err := f.st.dropSession(f.session, false)
			if (err != nil) != fail || f.session.dead == fail {
				t.Fatal(err, f.session.dead)
			}
			if len(f.sdk.Calls("users.kick")) != 1 {
				t.Fatal("未吊销真实设备")
			}
		})
	}
}

func TestDelayedGlobalRevocationPreservesFreshSession(t *testing.T) {
	f := setupDemo(t)
	f.sdk.Respond("users.sessions", 200, `{"items":[{"session_id":"real-session"}]}`)
	err := f.st.eventHandlers().UserReloginRequired(context.Background(), nil, &callback.UserReloginRequired{SessionsRevokedBefore: time.Now().Add(-time.Minute)})
	if err != nil || f.session.dead {
		t.Fatal("旧事件踢掉了有效会话", err)
	}
	f.sdk.Respond("users.sessions", 200, `{"items":[]}`)
	err = f.st.eventHandlers().UserReloginRequired(context.Background(), nil, &callback.UserReloginRequired{})
	if err != nil || !f.session.dead {
		t.Fatal("失效会话未被清理", err)
	}
}

func TestReplySnapshotCanBeMarshaledWhileCacheChanges(t *testing.T) {
	f := setupDemo(t)
	ctx := f.st.contextOf("alice")
	ctx.Friends["bob"] = map[string]any{"username": "bob", "nickname": "old"}
	out, err := f.st.dispatch(f.session, "friends", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_, _ = json.Marshal(out)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			f.st.mu.Lock()
			ctx.Friends["bob"]["nickname"] = "new"
			f.st.mu.Unlock()
		}
	}()
	wg.Wait()
	if out.([]map[string]any)[0]["nickname"] != "old" {
		t.Fatal("回复仍引用共享缓存")
	}
}

func TestMediaHeadersRejectActiveContent(t *testing.T) {
	for _, body := range []string{"<script>alert(1)</script>", "<svg xmlns='http://www.w3.org/2000/svg'></svg>", "file"} {
		w := httptest.NewRecorder()
		writeMediaHeaders(w, []byte(body))
		if w.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Fatal(w.Header())
		}
	}
	w := httptest.NewRecorder()
	writeMediaHeaders(w, []byte("\x89PNG\r\n\x1a\n"))
	if w.Header().Get("Content-Type") != "image/png" {
		t.Fatal(w.Header())
	}
}
