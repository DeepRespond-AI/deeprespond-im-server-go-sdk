package callback_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

const (
	secret     = "cbs_oSi8UFoUx4RQ8TlhuDzI06dQGntx4oJ6dOaloTrcCCE"
	oldSecret  = "cbs_H12F_317EerZFqcxUBYGdozmg-vYcs3bnRwLkaidZkI"
	endpointID = drimtest.DefaultEndpointID
)

var testdata = filepath.Join("..", "testdata")

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newHandler(t *testing.T, mutate func(*callback.Config)) (*callback.Handler, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	cfg := callback.Config{
		Secrets: callback.StaticSecrets{endpointID: {secret}},
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Logger:  slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := callback.NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h, logs
}

func post(h http.Handler, body []byte, secrets ...string) *httptest.ResponseRecorder {
	if len(secrets) == 0 {
		secrets = []string{secret}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, drimtest.SignedRequest(endpointID, body, secrets...))
	return rec
}

func TestSignatureVectors(t *testing.T) { // S-23、S-24
	vectors := drimtest.SignatureVectors()
	if len(vectors) < 10 {
		t.Fatal(len(vectors))
	}
	for _, v := range vectors {
		err := callback.Verify([]string{v.Secret}, v.Header(), v.Body(), time.Unix(v.Now, 0))
		if v.Valid {
			if err != nil {
				t.Errorf("%s：%v", v.Name, err)
			}
		} else {
			var verr *callback.VerifyError
			if !errors.As(err, &verr) || verr.Reason != v.Error {
				t.Errorf("%s：期望 %s，得到 %v", v.Name, v.Error, err)
			}
		}
		if len(v.SigningSecrets) == 0 && v.Expected != "" && v.SignatureHeader == "" {
			if got := "v1=" + callback.Sign(v.Secret, v.Timestamp, v.Nonce, v.Body()); got != v.Expected {
				t.Errorf("%s：签名 %s，期望 %s", v.Name, got, v.Expected)
			}
		}
	}
	// 文件与 testdata 中的相同（生成时复制）
	shared, err := os.ReadFile(filepath.Join(testdata, "signature_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fromFile []drimtest.SignatureVector
	if err := json.Unmarshal(shared, &fromFile); err != nil || len(fromFile) != len(vectors) {
		t.Fatal("drimtest 中的签名向量与 testdata 不同，运行 go generate")
	}
}

func TestVerifyURL(t *testing.T) { // S-29
	var called atomic.Int32
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.Any = func(context.Context, *callback.Event) error { called.Add(1); return nil }
	})
	rec := post(h, drimtest.VerifyRequest("J9Kk_hKp"))
	if rec.Code != 200 || rec.Body.String() != `{"challenge":"J9Kk_hKp"}` || called.Load() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestReplayAndStale(t *testing.T) { // S-25
	h, logs := newHandler(t, nil)
	body := drimtest.VerifyRequest("c")
	header := drimtest.SignedHeader(endpointID, body, time.Now().Add(-6*time.Minute), "0123456789abcdef0123456789abcdef", secret)
	status, _, _ := h.HandleRequest(context.Background(), header, body)
	if status != 401 {
		t.Fatal(status)
	}
	header = drimtest.SignedHeader(endpointID, body, time.Now(), "00000000000000000000000000000001", secret)
	if status, _, _ := h.HandleRequest(context.Background(), header, body); status != 200 {
		t.Fatal(status)
	}
	if status, _, _ := h.HandleRequest(context.Background(), header, body); status != 401 {
		t.Fatal("同一随机数第二次出现应拒绝", status)
	}
	if !strings.Contains(logs.String(), "stale_timestamp") || !strings.Contains(logs.String(), "重放") {
		t.Fatal(logs.String())
	}
}

func TestRotationOldSecretConfigured(t *testing.T) { // S-24
	h, _ := newHandler(t, func(c *callback.Config) { c.Secrets = callback.StaticSecrets{endpointID: {oldSecret}} })
	if rec := post(h, drimtest.VerifyRequest("c"), secret, oldSecret); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}

func TestDuplicateEvent(t *testing.T) { // S-26
	var calls atomic.Int32
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.UserCreated = func(ctx context.Context, ev *callback.Event, d *callback.UserCreated) error {
			calls.Add(1)
			if d.Username != "zhangsan" || callback.RequestFrom(ctx).App != drimtest.DefaultApp {
				t.Error("解析不对")
			}
			return nil
		}
	})
	event := drimtest.Event{ID: "1001", Type: "user.created", Data: map[string]any{"origin": "server", "username": "zhangsan", "created_via": "openapi"}}
	for i := 0; i < 2; i++ {
		if rec := post(h, drimtest.EventRequest(event)); rec.Code != 204 {
			t.Fatal(rec.Code, rec.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("重复的事件不再调用处理函数")
	}
}

func TestBatchFailure(t *testing.T) { // S-27
	var mu sync.Mutex
	var processed []string
	fail := true
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.Any = func(ctx context.Context, ev *callback.Event) error {
			mu.Lock()
			defer mu.Unlock()
			processed = append(processed, ev.ID)
			if ev.ID == "2" && fail {
				fail = false
				return callback.RetryAfter(30*time.Second, errors.New("数据库忙"))
			}
			return nil
		}
	})
	body := drimtest.EventRequest(drimtest.Event{ID: "1", Type: "x.a"}, drimtest.Event{ID: "2", Type: "x.b"}, drimtest.Event{ID: "3", Type: "x.c"})
	rec := post(h, body)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "30" {
		t.Fatal(rec.Code, rec.Header())
	}
	if rec := post(h, body); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if strings.Join(processed, ",") != "1,2,2,3" {
		t.Fatalf("重发时第 1 个被跳过，第 2、3 个重新处理：%v", processed)
	}
}

func TestConcurrentSameEvent(t *testing.T) { // S-28
	release := make(chan struct{})
	var calls atomic.Int32
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.Any = func(ctx context.Context, ev *callback.Event) error {
			calls.Add(1)
			<-release
			return nil
		}
	})
	body := drimtest.EventRequest(drimtest.Event{ID: "7", Type: "x.a"})
	first := make(chan int)
	go func() { first <- post(h, body).Code }()
	time.Sleep(100 * time.Millisecond)
	if rec := post(h, body); rec.Code != 503 {
		t.Fatal("被并发占用的事件返回 503", rec.Code)
	}
	close(release)
	if code := <-first; code != 204 || calls.Load() != 1 {
		t.Fatal(code, calls.Load())
	}
}

func TestQueueMode(t *testing.T) { // S-47
	var queued []string
	failed := false
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Queue = queueFunc(func(ctx context.Context, ev *callback.Event) error {
			if ev.ID == "2" && !failed {
				failed = true
				return errors.New("队列不可用")
			}
			queued = append(queued, ev.ID)
			return nil
		})
	})
	body := drimtest.EventRequest(drimtest.Event{ID: "1", Type: "x.a"}, drimtest.Event{ID: "2", Type: "x.b"}, drimtest.Event{ID: "3", Type: "x.c"})
	if rec := post(h, body); rec.Code != 503 {
		t.Fatal(rec.Code)
	}
	if strings.Join(queued, ",") != "1" {
		t.Fatalf("第 3 个不处理：%v", queued)
	}
	if rec := post(h, body); rec.Code != 204 || strings.Join(queued, ",") != "1,2,3" {
		t.Fatal(rec.Code, queued)
	}
	_, err := callback.NewHandler(callback.Config{
		Secrets: callback.StaticSecrets{}, Nonces: callback.NewMemoryNonceStore(), Dedup: callback.NewMemoryDedupStore(0),
		Queue:  queueFunc(func(context.Context, *callback.Event) error { return nil }),
		Events: callback.EventHandlers{MessageSent: func(context.Context, *callback.Event, *callback.MessageSent) error { return nil }},
	})
	if err == nil {
		t.Fatal("Events 与 Queue 二选一")
	}
}

type queueFunc func(ctx context.Context, ev *callback.Event) error

func (f queueFunc) Enqueue(ctx context.Context, ev *callback.Event) error { return f(ctx, ev) }

func TestUnknownEndpointAndMismatch(t *testing.T) { // S-48、S-49
	h, logs := newHandler(t, nil)
	body := drimtest.EventRequestFor("999", drimtest.Event{ID: "1", Type: "x.a"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, drimtest.SignedRequest("999", body, secret))
	if rec.Code != 401 || !strings.Contains(logs.String(), "999") {
		t.Fatal(rec.Code, logs.String())
	}
	// 请求体的 endpoint_id 与请求头不同
	body = drimtest.EventRequestFor("888", drimtest.Event{ID: "1", Type: "x.a"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, drimtest.SignedRequest(endpointID, body, secret))
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	// 请求头 X-IM-Kind 被改为 verify：按请求体的 kind 分派
	body = drimtest.EventRequest(drimtest.Event{ID: "5", Type: "x.a"})
	req := drimtest.SignedRequest(endpointID, body, secret)
	req.Header.Set("X-IM-Kind", "verify")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatal(rec.Code)
	}
}

func TestHTTPErrors(t *testing.T) {
	h, _ := newHandler(t, func(c *callback.Config) { c.MaxBodyBytes = 100 })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 405 {
		t.Fatal(rec.Code)
	}
	if rec := post(h, bytes.Repeat([]byte("a"), 101)); rec.Code != 413 {
		t.Fatal(rec.Code)
	}
	h2, _ := newHandler(t, nil)
	if rec := post(h2, []byte(`{"kind":"other"}`)); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	if rec := post(h2, []byte(`{"kind":"event","endpoint_id":"1840200000000000007","events":[{"type":"x"}]}`)); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	body := drimtest.VerifyRequest("c")
	req := drimtest.SignedRequest(endpointID, body, "cbs_wrong")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "bad_signature") {
		t.Fatal(rec.Code, rec.Body)
	}
}

func TestRawBodyPreserved(t *testing.T) { // S-32
	raw := `{"html":"<a href=\"x\">&","price":1.0,"text":"中文 😀","u":"<"}`
	var got []byte
	var ext []byte
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.MessageSent = func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
			got, ext = d.Body, d.Ext
			return nil
		}
	})
	data := `{"origin":"client","conversation_id":"1","conversation_type":"single","seq":1,"message_id":"9","client_msg_id":"k",` +
		`"sender_username":"a","sender_type":"user","recipient_username":"b","group_id":null,"type":"custom","body":` + raw +
		`,"ext":null,"mention_usernames":[],"mention_all":false,"reply_seq":null,"need_receipt":false,"exclude_from_unread":false,` +
		`"via":"client","created_at":"2026-10-06T00:00:00.000Z","content_available":true}`
	if rec := post(h, drimtest.EventRequest(drimtest.Event{ID: "1", Type: "message.sent", Data: data})); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if string(got) != raw || ext != nil {
		t.Fatalf("body 应与请求体中的逐字节相同：%s", got)
	}
}

func TestEventSamples(t *testing.T) { // S-33
	files, _ := filepath.Glob(filepath.Join(testdata, "events", "*.json"))
	if len(files) < 52 {
		t.Fatal(len(files))
	}
	seen := map[string]bool{}
	for _, file := range files {
		body, _ := os.ReadFile(file)
		req, err := callback.Parse(body)
		if err != nil {
			t.Fatalf("%s：%v", file, err)
		}
		for _, ev := range req.Events {
			d, err := ev.Decode()
			if errors.Is(err, callback.ErrUnknownEventType) {
				if ev.Type != "future.event_added" {
					t.Errorf("%s：不认识 %s", file, ev.Type)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s：%v", file, err)
				continue
			}
			seen[ev.Type] = true
			if fmt.Sprintf("%T", d) == "" {
				t.Fatal()
			}
		}
	}
	if len(seen) != len(callback.EventTypes) || len(callback.EventTypes) != 61 {
		t.Fatalf("样本覆盖 %d 种，SDK 认识 %d 种", len(seen), len(callback.EventTypes))
	}
	// 截短、微秒时间、origin 为 null、不认识的事件和字段
	var unknown, members, truncated, sessions int
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Events.Unknown = func(ctx context.Context, ev *callback.Event) error { unknown++; return nil }
		c.Events.GroupMembersAdded = func(ctx context.Context, ev *callback.Event, d *callback.GroupMembersAdded) error {
			if d.Members[0].Username == nil || *d.Members[0].Username != "wangwu" || !strings.Contains(string(d.Raw), "future_field") {
				t.Error("不认识的字段不报错，Raw 中可以读到")
			}
			members++
			return nil
		}
		c.Events.MessageSent = func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
			if d.Truncated {
				if d.Body != nil {
					t.Error("截短后 body 为 nil")
				}
				truncated++
			}
			return nil
		}
		c.Events.PresenceChanged = func(ctx context.Context, ev *callback.Event, d *callback.PresenceChanged) error {
			if d.Origin != nil {
				t.Error("在线状态的 origin 为 nil")
			}
			return nil
		}
		c.Events.UserSessionCreated = func(ctx context.Context, ev *callback.Event, d *callback.UserSessionCreated) error {
			if d.CreatedAt.Nanosecond()%1000 != 0 || d.CreatedAt.Nanosecond()/1000%1000 == 0 {
				t.Errorf("微秒时间：%v", d.CreatedAt)
			}
			sessions++
			return nil
		}
	})
	for _, name := range []string{"_unknown.json", "_truncated.json", "_batch.json"} {
		body, _ := os.ReadFile(filepath.Join(testdata, "events", name))
		rec := httptest.NewRecorder()
		req := drimtest.SignedRequest(endpointID, body, secret)
		h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("%s：%d %s", name, rec.Code, rec.Body)
		}
	}
	if unknown != 1 || members != 1 || truncated != 1 || sessions != 1 {
		t.Fatal(unknown, members, truncated, sessions)
	}
}

func hookData(t *testing.T) []byte {
	t.Helper()
	return drimtest.HookRequestFor(endpointID, "message.before_send", 300, map[string]any{
		"via": "client", "client_msg_id": "k", "conversation_type": "single", "sender_username": "a", "recipient_username": "b",
		"recipient_usernames": nil, "group_id": nil, "type": "text", "body": map[string]any{"text": "广告"}, "ext": nil,
		"mention_usernames": []string{}, "mention_all": false, "reply_seq": nil, "need_receipt": false, "online_only": false, "edit": nil,
	})
}

func TestHookResults(t *testing.T) { // S-30、S-31
	mode := "reject"
	h, logs := newHandler(t, func(c *callback.Config) {
		c.Hooks.MessageBeforeSend = func(ctx context.Context, req *callback.MessageBeforeSend) (callback.Verdict, error) {
			if req.Timeout != 300*time.Millisecond || string(req.Body) != `{"text":"广告"}` {
				t.Errorf("请求：%+v", req)
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 250*time.Millisecond {
				t.Error("截止时间为 timeout_ms 减 50 毫秒")
			}
			switch mode {
			case "reject":
				return callback.Reject("ad", "消息中有<不允许>的内容"), nil
			case "modify":
				return callback.Modify(drim.JSON(`{ "text": "***" }`), drim.Null[json.RawMessage]()), nil
			case "newline":
				return callback.Reject("ad", "第一行\n第二行"), nil
			case "slow":
				time.Sleep(500 * time.Millisecond)
				return callback.Allow(), nil
			case "error":
				return callback.Verdict{}, errors.New("数据库忙")
			case "panic":
				panic("出错了")
			}
			return callback.Verdict{}, nil
		}
	})
	rec := post(h, hookData(t))
	if rec.Code != 200 || rec.Body.String() != `{"action":"reject","reason":"ad","message":"消息中有<不允许>的内容"}` {
		t.Fatal(rec.Code, rec.Body)
	}
	mode = "modify"
	if rec := post(h, hookData(t)); rec.Body.String() != `{"action":"modify","body":{"text":"***"},"ext":null}` {
		t.Fatal(rec.Body)
	}
	for _, m := range []string{"newline", "error", "panic", "zero"} {
		mode = m
		if rec := post(h, hookData(t)); rec.Code != 503 {
			t.Fatalf("%s：%d", m, rec.Code)
		}
	}
	if !strings.Contains(logs.String(), "结果不合规") {
		t.Fatal(logs.String())
	}
	mode = "slow"
	started := time.Now()
	if rec := post(h, hookData(t)); rec.Code != 503 || time.Since(started) > 300*time.Millisecond {
		t.Fatalf("超时在截止时间返回：%d %v", rec.Code, time.Since(started))
	}
	fallback := callback.Allow()
	h2, _ := newHandler(t, func(c *callback.Config) {
		c.HookFallback = &fallback
		c.Hooks.MessageBeforeSend = func(ctx context.Context, req *callback.MessageBeforeSend) (callback.Verdict, error) {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			return callback.Reject("", ""), nil
		}
	})
	if rec := post(h2, hookData(t)); rec.Code != 200 || rec.Body.String() != `{"action":"allow"}` {
		t.Fatal(rec.Code, rec.Body)
	}
	// 没有设置处理函数的同步回调
	friend := drimtest.HookRequestFor(endpointID, "friend.before_add", 1000, map[string]any{"stage": "request", "from_username": "a", "to_username": "b"})
	if rec := post(h2, friend); rec.Code != 200 || rec.Body.String() != `{"action":"allow"}` {
		t.Fatal(rec.Code, rec.Body)
	}
}

func TestPartialVerdict(t *testing.T) {
	h, _ := newHandler(t, func(c *callback.Config) {
		c.Hooks.GroupBeforeJoin = func(ctx context.Context, req *callback.GroupBeforeJoin) (callback.Verdict, error) {
			if req.Group.GroupID != nil || req.JoinedVia != "create" {
				t.Errorf("%+v", req)
			}
			return callback.Partial(callback.RejectedTarget{Username: "lisi", Reason: "not_same_org"}), nil
		}
	})
	body := drimtest.HookRequestFor(endpointID, "group.before_join", 1000, map[string]any{
		"joined_via": "create", "group": map[string]any{"group_id": nil, "type": "private", "name": "g"},
		"operator": "a", "targets": []string{"lisi", "wangwu"}, "message": "", "link_creator": nil,
	})
	rec := post(h, body)
	if rec.Code != 200 || rec.Body.String() != `{"action":"partial","rejected":[{"username":"lisi","reason":"not_same_org"}]}` {
		t.Fatal(rec.Code, rec.Body)
	}
}

func TestMemoryStores(t *testing.T) {
	ctx := context.Background()
	d := callback.NewMemoryDedupStore(time.Hour)
	state, token, _ := d.Claim(ctx, "e1", time.Minute)
	if state != callback.Acquired || token == "" {
		t.Fatal(state)
	}
	if state, _, _ := d.Claim(ctx, "e1", time.Minute); state != callback.Busy {
		t.Fatal(state)
	}
	_ = d.Release(ctx, "e1", "wrong-token")
	if state, _, _ := d.Claim(ctx, "e1", time.Minute); state != callback.Busy {
		t.Fatal("令牌不同不释放")
	}
	_ = d.Complete(ctx, "e1", token)
	if state, _, _ := d.Claim(ctx, "e1", time.Minute); state != callback.Done {
		t.Fatal(state)
	}
	state, token2, _ := d.Claim(ctx, "e2", time.Minute)
	_ = d.Release(ctx, "e2", token2)
	if state2, _, _ := d.Claim(ctx, "e2", time.Minute); state != callback.Acquired || state2 != callback.Acquired {
		t.Fatal("释放后可以重新占用")
	}
	n := callback.NewMemoryNonceStore()
	if seen, _ := n.Seen(ctx, "a", time.Minute); seen {
		t.Fatal()
	}
	if seen, _ := n.Seen(ctx, "a", time.Minute); !seen {
		t.Fatal()
	}
}
