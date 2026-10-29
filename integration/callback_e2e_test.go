//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

// receiver 记录收到的事件和同步回调；密钥在新建地址后才知道。
type receiver struct {
	mu          sync.Mutex
	secrets     map[string][]string
	events      map[string]string // 事件 ID → 类型
	order       []string
	attempts    map[string]int
	sentBodies  []json.RawMessage
	hooks       int
	failedEvent string
	duplicates  int
}

func (r *receiver) Secrets(_ context.Context, endpointID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secrets[endpointID], nil
}

func (r *receiver) record(ev *callback.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Type == "friend.added" && r.failedEvent == "" {
		// 第一次处理好友事件时出错：这一批返回 503，服务端按重试计划重发整批
		r.failedEvent = ev.ID
		return errors.New("模拟数据库故障")
	}
	if _, ok := r.events[ev.ID]; ok {
		r.duplicates++
	}
	r.events[ev.ID] = ev.Type
	r.order = append(r.order, ev.ID)
	r.attempts[ev.ID] = ev.Attempt
	return nil
}

func (r *receiver) has(check func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return check()
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func consoleCall(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	return control(t, http.MethodPost, "/console", map[string]any{"method": method, "path": "/apps/" + env.AppName + "/callback" + path, "body": body})
}

func TestCallbacksEndToEnd(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	rcv := &receiver{secrets: map[string][]string{}, events: map[string]string{}, attempts: map[string]int{}}
	handler, err := callback.NewHandler(callback.Config{
		Secrets: rcv,
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Events: callback.EventHandlers{
			UserCreated: func(ctx context.Context, ev *callback.Event, d *callback.UserCreated) error { return rcv.record(ev) },
			FriendAdded: func(ctx context.Context, ev *callback.Event, d *callback.FriendAdded) error { return rcv.record(ev) },
			MessageSent: func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
				rcv.mu.Lock()
				rcv.sentBodies = append(rcv.sentBodies, d.Body)
				rcv.mu.Unlock()
				return rcv.record(ev)
			},
		},
		Hooks: callback.HookHandlers{
			MessageBeforeSend: func(ctx context.Context, req *callback.MessageBeforeSend) (callback.Verdict, error) {
				rcv.mu.Lock()
				rcv.hooks++
				rcv.mu.Unlock()
				var body struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(req.Body, &body)
				switch body.Text {
				case "bad":
					return callback.Reject("forbidden_word", "消息中有不允许的内容"), nil
				case "modify":
					return callback.Modify(drim.JSON(`{"text":"***"}`), drim.Field[json.RawMessage]{}), nil
				case "slow":
					<-ctx.Done()
					time.Sleep(2 * time.Second)
				}
				return callback.Allow(), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	url := server.URL + "/im/callback"

	consoleCall(t, http.MethodGet, "/endpoints", nil)   // 控制台的会话过期时 harness 先重新登录
	control(t, http.MethodPost, "/console/reauth", nil) // 新建地址、设置同步回调要求 10 分钟内重新验证过身份
	created := consoleCall(t, http.MethodPost, "/endpoints", map[string]any{
		"name": uniq("sdk-go-e2e-"), "url": url, "event_types": []string{"user.created", "message.sent", "friend.added"},
		"batch_size": 10, "timeout_ms": 5000,
	})
	endpoint, ok := created["endpoint"].(map[string]any)
	if !ok {
		t.Fatalf("新建回调地址失败：%v", created)
	}
	endpointID := endpoint["endpoint_id"].(string)
	rcv.mu.Lock()
	rcv.secrets[endpointID] = []string{created["secret"].(string)}
	rcv.mu.Unlock()
	defer consoleCall(t, http.MethodDelete, "/endpoints/"+endpointID, nil)

	verification := consoleCall(t, http.MethodPost, "/endpoints/"+endpointID+"/verify", nil)
	if verification["ok"] != true && verification["status"] != "verified" && verification["status"] != "ok" {
		t.Fatalf("URL 验证失败：%v", verification)
	}

	a, b := uniq("cb"), uniq("cb")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: a}))
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: b}))
	raw := `{"price":1.0,"html":"<b>&</b>"}`
	must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "custom", Body: drim.JSON(raw), ClientMsgID: uniq("k")}))
	must(im.Friends.Add(ctx, a, b, drim.AddFriendParams{}))
	types := func() bool {
		seen := map[string]bool{}
		for _, typ := range rcv.events {
			seen[typ] = true
		}
		return seen["user.created"] && seen["message.sent"] && seen["friend.added"]
	}
	// 好友事件第一次处理失败，服务端约 10 秒后重发（重试计划的第一档）
	check(t, waitFor(30*time.Second, func() bool { return rcv.has(types) }), "没有收到全部事件：%v", rcv.events)
	check(t, waitFor(30*time.Second, func() bool {
		return rcv.has(func() bool { _, ok := rcv.attempts[rcv.failedEvent]; return rcv.failedEvent != "" && ok })
	}), "失败的事件没有被重发")
	rcv.mu.Lock()
	check(t, rcv.attempts[rcv.failedEvent] >= 2, "是服务端重发的：%d", rcv.attempts[rcv.failedEvent])
	check(t, rcv.duplicates == 0, "重发的整批中已处理的事件被去重，没有处理两次：%d", rcv.duplicates)
	found := false
	for _, body := range rcv.sentBodies {
		found = found || string(body) == raw
	}
	check(t, found, "消息抄送的 body 与发送的逐字节相同：%s", rcv.sentBodies)
	rcv.mu.Unlock()

	// 同步回调：开启发消息前回调，对 OpenAPI 的发送也调用
	hooks := consoleCall(t, http.MethodGet, "/hooks", nil)
	var version any
	for _, item := range hooks["items"].([]any) {
		if h := item.(map[string]any); h["hook"] == "message.before_send" {
			version = h["version"]
		}
	}
	hook := consoleCall(t, http.MethodPut, "/hooks/message.before_send", map[string]any{
		"enabled": true, "endpoint_id": endpointID, "timeout_ms": 1000, "on_failure": "allow",
		"conditions": map[string]any{"vias": []string{"openapi"}}, "version": version,
	})
	check(t, hook["enabled"] == true, "开启同步回调：%v", hook)
	defer consoleCall(t, http.MethodPut, "/hooks/message.before_send", map[string]any{"enabled": false, "version": hook["version"]})
	// 服务端的同步回调设置在进程内的快照中，改动后生效的时间不固定：反复发送，直到被拒绝
	var rejected error
	waitFor(15*time.Second, func() bool {
		_, err := im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "text", Body: drim.JSON(`{"text":"bad"}`)})
		rejected = err
		return err != nil
	})
	check(t, drim.IsReason(rejected, drim.CodeMessageRejected, "app_rejected"), "同步回调的拒绝：%v", rejected)
	var e *drim.Error
	errors.As(rejected, &e)
	check(t, e.Details["app_reason"] == "forbidden_word", "%v", e.Details)
	modified := must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "text", Body: drim.JSON(`{"text":"modify"}`)}))
	check(t, string(modified.Body) == `{"text":"***"}`, "修改：%s", modified.Body)
	started := time.Now()
	slow := must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "text", Body: drim.JSON(`{"text":"slow"}`)}))
	check(t, strings.Contains(string(slow.Body), "slow") && time.Since(started) < 2*time.Second, "超时按失败策略放行：%s %v", slow.Body, time.Since(started))
	check(t, rcv.has(func() bool { return rcv.hooks >= 3 }), "同步回调的次数：%d", rcv.hooks)
	_ = im.Users.Delete(ctx, a)
	_ = im.Users.Delete(ctx, b)
}
