package drim_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

// dropConnection 在服务端读完请求后断开连接（请求已发出，客户端收不到响应）。
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func TestSingleFlightToken(t *testing.T) { // S-01
	srv := newServer(t)
	var exchanges atomic.Int32
	srv.Handle("app.token", func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte(`{"access_token":"x","expires_in":3600,"application":"1"}`))
	})
	client := newClient(t, srv)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Token(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := exchanges.Load(); n != 1 {
		t.Fatalf("100 个协程只应换取一次，换取了 %d 次", n)
	}
}

func TestBackgroundRefresh(t *testing.T) { // S-02
	srv := newServer(t)
	srv.TokenTTL = 4 * time.Minute // 剩余不到 5 分钟：下一个请求触发后台换取
	client := newClient(t, srv)
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	calls := srv.Calls("users.get")
	if calls[1].Header.Get("Authorization") != "Bearer fake-token-1" {
		t.Fatal("后台换取期间请求仍用旧令牌")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(srv.Calls("app.token")) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.Calls("app.token")) != 2 {
		t.Fatal("应当在后台换取新令牌")
	}
	time.Sleep(20 * time.Millisecond)
	_, _ = client.Users.Get(context.Background(), "a")
	if got := srv.Calls("users.get")[2].Header.Get("Authorization"); got != "Bearer fake-token-2" {
		t.Fatal(got)
	}
}

func TestTokenRevoked(t *testing.T) { // S-03
	srv := newServer(t)
	client := newClient(t, srv)
	_, _ = client.Users.Get(context.Background(), "a")
	srv.RevokeTokens()
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if len(srv.Calls("app.token")) != 2 || len(srv.Calls("users.get")) != 3 {
		t.Fatalf("换取后重发一次：%d %d", len(srv.Calls("app.token")), len(srv.Calls("users.get")))
	}
	// 重发仍是 unauthenticated：返回错误
	srv.Fail("users.get", "unauthenticated", drimtest.FailOptions{Times: 2})
	_, err := client.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeUnauthenticated || e.Attempts != 2 {
		t.Fatalf("%+v", e)
	}
}

func TestAppUnavailableCooldown(t *testing.T) { // S-04
	srv := newServer(t)
	logs := &logBuffer{}
	client := newClient(t, srv, func(c *drim.Config) { c.Logger = logs.logger() })
	_, _ = client.Users.Get(context.Background(), "a")
	srv.RevokeTokens()
	srv.Fail("app.token", "app_unavailable", drimtest.FailOptions{Times: -1})
	_, err := client.Users.Get(context.Background(), "a")
	e := asError(t, err)
	if e.Code != drim.CodeAppUnavailable || e.Op != "app.token" || !drim.IsServiceUnavailable(err) {
		t.Fatalf("%+v", e)
	}
	exchanges := len(srv.Calls("app.token"))
	_, err = client.Users.Get(context.Background(), "a")
	e = asError(t, err)
	if e.Code != drim.CodeClientUnavailable || e.Details["cause"] != "app_unavailable" || !drim.IsServiceUnavailable(err) {
		t.Fatalf("%+v", e)
	}
	until, _ := e.Details["until"].(time.Time)
	if d := time.Until(until); d < 110*time.Second || d > 300*time.Second {
		t.Fatalf("冷却 2 到 5 分钟：%v", d)
	}
	if len(srv.Calls("app.token")) != exchanges {
		t.Fatal("冷却期内不再换取")
	}
	if !strings.Contains(logs.String(), "冷却期内不再换取") {
		t.Fatal(logs.String())
	}
}

func TestPermanentlyUnavailable(t *testing.T) { // S-05
	srv := newServer(t)
	client := newClient(t, srv)
	srv.Fail("users.get", "unauthenticated", drimtest.FailOptions{Details: map[string]any{"reason": "app_deleted"}})
	_, err := client.Users.Get(context.Background(), "a")
	if !drim.IsPermanentlyUnavailable(err) {
		t.Fatal(err)
	}
	before := len(srv.Requests())
	_, err = client.Groups.Get(context.Background(), "1")
	if e := asError(t, err); e.Code != drim.CodeClientUnavailable || !drim.IsPermanentlyUnavailable(err) {
		t.Fatal(e)
	}
	if len(srv.Requests()) != before {
		t.Fatal("永久失效后不再发请求")
	}
}

func TestInvalidClientCooldownAndUpdate(t *testing.T) { // S-06、S-42
	srv := newServer(t)
	client := newClient(t, srv, func(c *drim.Config) { c.ClientSecret = "wrong" })
	_, err := client.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeInvalidClient {
		t.Fatal(e)
	}
	for i := 0; i < 5; i++ {
		_, err = client.Users.Get(context.Background(), "a")
		if e := asError(t, err); e.Code != drim.CodeClientUnavailable || e.Details["cause"] != "invalid_client" {
			t.Fatal(e)
		}
	}
	if n := len(srv.Calls("app.token")); n != 1 {
		t.Fatalf("1 分钟内不再换取：%d", n)
	}
	client.UpdateCredentials(srv.ClientID, srv.ClientSecret)
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
}

func TestSendRetriedWithSameKey(t *testing.T) { // S-08
	srv := newServer(t)
	var mu sync.Mutex
	seen := map[string]int{}
	srv.Handle("messages.send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientMsgID string `json:"client_msg_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen[body.ClientMsgID]++
		n := seen[body.ClientMsgID]
		mu.Unlock()
		if n == 1 {
			dropConnection(w) // 服务端已经保存，响应在途中丢失
			return
		}
		w.WriteHeader(200) // 重复提交
		w.Write([]byte(`{"message_id":"1","seq":1}`))
	})
	client := newClient(t, srv)
	res, err := client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate || len(seen) != 1 {
		t.Fatalf("应当用同一个 client_msg_id 重发：%v", seen)
	}
}

func TestUnsafeNotRetried(t *testing.T) { // S-09、S-10、S-52
	srv := newServer(t)
	srv.Handle("groups.create", func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	srv.Handle("users.set_password", func(w http.ResponseWriter, r *http.Request) { dropConnection(w) })
	srv.Handle("users.kick_all", func(w http.ResponseWriter, r *http.Request) { dropConnection(w) })
	srv.Handle("messages.send", func(w http.ResponseWriter, r *http.Request) { dropConnection(w) })
	client := newClient(t, srv)
	ctx := drim.WithAttemptTimeout(context.Background(), 100*time.Millisecond)
	_, err := client.Groups.Create(ctx, drim.CreateGroupParams{Owner: "a"})
	e := asError(t, err)
	if e.Code != drim.CodeTimeout || !errors.Is(err, context.DeadlineExceeded) || len(srv.Calls("groups.create")) != 1 {
		t.Fatalf("建群超时不重试：%+v", e)
	}
	err = client.Users.SetPassword(context.Background(), "a", "p")
	if e := asError(t, err); e.Code != drim.CodeNetworkError || len(srv.Calls("users.set_password")) != 1 {
		t.Fatal(e)
	}
	err = client.Users.KickAll(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeNetworkError || len(srv.Calls("users.kick_all")) != 1 {
		t.Fatal(e)
	}
	_, err = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "custom", Body: drim.JSON(`{}`), OnlineOnly: true})
	if e := asError(t, err); e.Code != drim.CodeNetworkError || len(srv.Calls("messages.send")) != 1 {
		t.Fatal(e)
	}
}

// dialFailing 是第一次返回连接失败（请求没有发出）的 RoundTripper。
type dialFailing struct {
	next  http.RoundTripper
	fails atomic.Int32
}

func (d *dialFailing) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/token") {
		return d.next.RoundTrip(r)
	}
	if d.fails.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return d.next.RoundTrip(r)
}

func TestUnsafeRetriedWhenNotSent(t *testing.T) { // S-11
	srv := newServer(t)
	rt := &dialFailing{next: srv.Config().HTTPClient.Transport}
	rt.fails.Store(1)
	client := newClient(t, srv, func(c *drim.Config) { c.HTTPClient = &http.Client{Transport: rt} })
	if _, err := client.Groups.Create(context.Background(), drim.CreateGroupParams{Owner: "a"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.Calls("groups.create")) != 1 {
		t.Fatal("连接没有建立的请求一定没有发出，可以重试")
	}
}

func TestSharedPause(t *testing.T) { // S-12
	srv := newServer(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	var firstLimited time.Time
	var after []time.Time
	// 每个协程的第一次尝试（按 X-Request-ID 区分，重试沿用同一个 ID）得到 rate_limited
	srv.Handle("users.get", func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		mu.Lock()
		first := !seen[id]
		seen[id] = true
		now := time.Now()
		if first && (firstLimited.IsZero() || now.Before(firstLimited)) {
			firstLimited = now
		}
		if !first {
			after = append(after, now)
		}
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.Write(drimtest.ErrorBody("rate_limited", "超出额度", nil))
			return
		}
		w.Write([]byte(`{}`))
	})
	client := newClient(t, srv)
	_, _ = client.Token(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := drim.WithRequestID(context.Background(), fmt.Sprintf("worker-%d", i))
			if _, err := client.Users.Get(ctx, "a"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(after) != 20 {
		t.Fatalf("%d", len(after))
	}
	for _, at := range after {
		// 暂停时刻只往后推：任何重发都不早于第一次被限流的 1 秒之后，不各自立即重试
		if at.Sub(firstLimited) < 950*time.Millisecond {
			t.Fatalf("所有请求都应等到暂停时刻之后：%v", at.Sub(firstLimited))
		}
	}
	if wait := client.Stats().Ops["users.get"].RateLimitWait; wait < 10*time.Second {
		t.Fatalf("统计中的等待时间：%v", wait)
	}
}

func TestRateLimitLimits(t *testing.T) { // S-13、S-37、S-38
	srv := newServer(t)
	client := newClient(t, srv)
	srv.Fail("users.get", "rate_limited", drimtest.FailOptions{RetryAfter: "3600"})
	started := time.Now()
	_, err := client.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeRateLimited || e.RetryAfter != time.Hour || time.Since(started) > time.Second {
		t.Fatalf("Retry-After 超过上限时不等待：%+v", e)
	}
	// 没有 Retry-After：按 1 秒；连续 4 次被限流：第 4 次后返回错误
	srv.Fail("groups.get", "rate_limited", drimtest.FailOptions{Times: 4})
	started = time.Now()
	_, err = client.Groups.Get(context.Background(), "1")
	if e := asError(t, err); e.Code != drim.CodeRateLimited || e.Attempts != 4 {
		t.Fatalf("%+v", e)
	}
	if d := time.Since(started); d < 3*time.Second || d > 5*time.Second {
		t.Fatalf("3 次各等 1 秒左右：%v", d)
	}
	// ctx 的截止时间在 Retry-After 之前：立即返回
	srv.Fail("friends.get", "rate_limited", drimtest.FailOptions{RetryAfter: "5"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started = time.Now()
	_, err = client.Friends.Get(ctx, "a", "b")
	if e := asError(t, err); e.Code != drim.CodeRateLimited || time.Since(started) > time.Second {
		t.Fatalf("%+v %v", e, time.Since(started))
	}
	// WithMaxRetryAfter(0)：不等待
	srv.Fail("friends.get", "rate_limited", drimtest.FailOptions{RetryAfter: "1"})
	_, err = client.Friends.Get(drim.WithMaxRetryAfter(context.Background(), 0), "a", "b")
	if e := asError(t, err); e.Code != drim.CodeRateLimited {
		t.Fatal(e)
	}
}

func TestRateLimitScopes(t *testing.T) { // S-14、S-35、S-36
	srv := newServer(t)
	client := newClient(t, srv)
	_, _ = client.Token(context.Background())
	// group_send_rate：只有这个请求等待
	srv.Fail("messages.send", "rate_limited", drimtest.FailOptions{RetryAfter: "1", Details: map[string]any{"reason": "group_send_rate"}})
	done := make(chan time.Duration)
	go func() {
		started := time.Now()
		_, err := client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToGroup: "g", Type: "text", Body: drim.JSON(`{}`)})
		if err != nil {
			t.Error(err)
		}
		done <- time.Since(started)
	}()
	time.Sleep(200 * time.Millisecond)
	started := time.Now()
	if _, err := client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToGroup: "g2", Type: "text", Body: drim.JSON(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 700*time.Millisecond {
		t.Fatal("别的群的消息不应等待")
	}
	if d := <-done; d < time.Second {
		t.Fatalf("被限流的请求应等待：%v", d)
	}
	// app_message_rate：发消息的请求一起等待（1 到 2 倍），查询不受影响
	srv.Fail("messages.send", "rate_limited", drimtest.FailOptions{RetryAfter: "1", Details: map[string]any{"reason": "app_message_rate"}})
	go func() {
		started := time.Now()
		_, _ = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(`{}`)})
		done <- time.Since(started)
	}()
	time.Sleep(200 * time.Millisecond)
	started = time.Now()
	if _, err := client.Users.Get(context.Background(), "a"); err != nil || time.Since(started) > 700*time.Millisecond {
		t.Fatal("查询不受发消息限流的影响", err)
	}
	started = time.Now()
	_, _ = client.Messages.BatchSend(context.Background(), drim.BatchSendParams{From: "a", ToUsers: []string{"b"}, ClientMsgID: "k", Type: "text", Body: drim.JSON(`{}`)})
	if time.Since(started) < 700*time.Millisecond {
		t.Fatal("发消息的请求应一起等待")
	}
	if d := <-done; d < time.Second || d > 2300*time.Millisecond {
		t.Fatalf("app_message_rate 在 Retry-After 到 2 倍之间等待：%v", d)
	}
}

func TestRetriedFlag(t *testing.T) { // S-39
	srv := newServer(t)
	var n atomic.Int32
	srv.Handle("users.create", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			dropConnection(w)
			return
		}
		w.WriteHeader(409)
		w.Write(drimtest.ErrorBody("already_exists", "用户名已被占用", nil))
	})
	client := newClient(t, srv)
	_, err := client.Users.Create(context.Background(), drim.CreateUserParams{Username: "zhangsan"})
	e := asError(t, err)
	if e.Code != drim.CodeAlreadyExists || !e.Retried || e.Attempts != 2 || !strings.Contains(e.Error(), "retried") {
		t.Fatalf("%+v", e)
	}
}

func TestRequestID(t *testing.T) { // S-40
	srv := newServer(t)
	client := newClient(t, srv)
	for _, id := range []string{strings.Repeat("a", 65), "has space", "中文"} {
		_, err := client.Users.Get(drim.WithRequestID(context.Background(), id), "a")
		if e := asError(t, err); e.Code != drim.CodeInvalidParams || e.Details["field"] != "request_id" {
			t.Fatal(e)
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("不应发请求")
	}
	var n atomic.Int32
	srv.Handle("users.get", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		w.Write([]byte(`{}`))
	})
	ctx := drim.WithAttemptTimeout(drim.WithRequestID(context.Background(), "order-svc.7f3a_9c"), 100*time.Millisecond)
	if _, err := client.Users.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	calls := srv.Calls("users.get")
	if len(calls) != 2 || calls[0].Header.Get("X-Request-ID") != "order-svc.7f3a_9c" || calls[1].Header.Get("X-Request-ID") != "order-svc.7f3a_9c" {
		t.Fatalf("重试沿用同一个 ID：%d", len(calls))
	}
}

func TestMaxAttemptsOption(t *testing.T) {
	srv := newServer(t)
	srv.Fail("users.get", "internal", drimtest.FailOptions{Times: 5})
	client := newClient(t, srv)
	_, err := client.Users.Get(drim.WithMaxAttempts(context.Background(), 1), "a")
	if e := asError(t, err); e.Attempts != 1 || len(srv.Calls("users.get")) != 1 {
		t.Fatal(e)
	}
	disabled := newClient(t, srv, func(c *drim.Config) { c.Retry.Disabled = true })
	_, err = disabled.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Attempts != 1 {
		t.Fatal(e)
	}
}

func TestCanceledContext(t *testing.T) {
	srv := newServer(t)
	srv.Handle("users.get", func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) })
	client := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := client.Users.Get(ctx, "a")
	if e := asError(t, err); e.Code != drim.CodeCanceled || !errors.Is(err, context.Canceled) {
		t.Fatal(e)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, err = client.Users.Get(ctx, "a")
	if e := asError(t, err); e.Code != drim.CodeTimeout || !errors.Is(err, context.DeadlineExceeded) || e.Attempts != 1 {
		t.Fatal(e)
	}
}

func TestHooks(t *testing.T) { // S-50
	srv := newServer(t)
	srv.Fail("users.get", "rate_limited", drimtest.FailOptions{RetryAfter: "1"})
	var mu sync.Mutex
	var infos []drim.RequestInfo
	var before []string
	client := newClient(t, srv, func(c *drim.Config) {
		c.Hooks = drim.Hooks{
			BeforeRequest: func(ctx context.Context, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				req.Header.Set("X-Trace", "t1")
				before = append(before, req.URL.Path)
			},
			AfterResponse: func(ctx context.Context, info drim.RequestInfo) {
				mu.Lock()
				defer mu.Unlock()
				infos = append(infos, info)
			},
		}
	})
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 || infos[0].Op != "app.token" || infos[1].Code != "rate_limited" || infos[2].Attempt != 2 {
		t.Fatalf("%+v", infos)
	}
	if infos[2].RateLimitWait < time.Second || infos[1].StatusCode != 429 || infos[2].StatusCode != 200 || len(before) != 3 {
		t.Fatalf("%+v", infos)
	}
	if srv.Calls("users.get")[0].Header.Get("X-Trace") != "t1" {
		t.Fatal("钩子可以注入追踪头")
	}
}

func TestDecideRetryRules(t *testing.T) { // S-51
	srv := newServer(t)
	var n atomic.Int32
	srv.Handle("moderation.decide", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%2 == 1 {
			dropConnection(w)
			return
		}
		w.WriteHeader(409)
		w.Write(drimtest.ErrorBody("version_conflict", "记录已变化", map[string]any{"current": map[string]any{"version": 2}}))
	})
	client := newClient(t, srv)
	actions := []drim.ModerationActionInput{{Kind: "recall_message"}}
	_, err := client.Moderation.Decide(context.Background(), "1", drim.DecideParams{Decision: "violation", Actions: actions})
	if e := asError(t, err); e.Code != drim.CodeNetworkError || len(srv.Calls("moderation.decide")) != 1 {
		t.Fatalf("带处置不带 version 的不重试：%+v", e)
	}
	n.Store(0)
	_, err = client.Moderation.Decide(context.Background(), "1", drim.DecideParams{Decision: "violation", Actions: actions, Version: drim.Int64(1)})
	if e := asError(t, err); e.Code != drim.CodeVersionConflict || !e.Retried || len(srv.Calls("moderation.decide")) != 3 {
		t.Fatalf("带 version 的重试：%+v", e)
	}
}

func TestBatchDecideRetried(t *testing.T) {
	srv := newServer(t)
	var n atomic.Int32
	srv.Handle("moderation.batch_decide", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			dropConnection(w)
			return
		}
		w.Write([]byte(`{"results":[{"item_id":"1","error":{"code":"version_conflict","message":"已结束","details":{"current":` +
			`{"item_id":"1","status":"violation","decision":{"result":"violation"},"version":3}}}},` +
			`{"item_id":"2","error":{"code":"version_conflict","details":{"current":{"item_id":"2","decision":{"result":"no_violation"}}}}}]}`))
	})
	client := newClient(t, srv)
	res, err := client.Moderation.BatchDecide(context.Background(), drim.BatchDecideParams{ItemIDs: []string{"1", "2"}, Decision: "violation"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Results[0].Error != nil || res.Results[0].Item == nil || res.Results[0].Item.Version != 3 {
		t.Fatalf("重试后结论相同的按成功处理：%+v", res.Results[0])
	}
	if res.Results[1].Error == nil || res.Results[1].Error.Key != "2" {
		t.Fatalf("%+v", res.Results[1])
	}
}

func TestBatchSendAll(t *testing.T) { // S-53
	srv := newServer(t)
	var mu sync.Mutex
	delivered := map[string]int{}
	var calls int
	srv.Handle("messages.batch_send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ToUsers     []string `json:"to_users"`
			ClientMsgID string   `json:"client_msg_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		calls++
		if len(body.ToUsers) > 600 {
			w.WriteHeader(400)
			w.Write(drimtest.ErrorBody("invalid_argument", "接收人过多", map[string]any{"reason": "too_many_items", "max": 600, "field": "to_users"}))
			return
		}
		var items []string
		for _, u := range body.ToUsers {
			status := "sent"
			if delivered[u] > 0 {
				status = "duplicate"
			}
			delivered[u]++
			items = append(items, fmt.Sprintf(`{"username":%q,"status":%q,"message_id":"m-%s"}`, u, status, u))
		}
		if calls == 3 { // 第二批：服务端已发出，响应在途中丢失
			dropConnection(w)
			return
		}
		fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(items, ","))
	})
	client := newClient(t, srv)
	users := make([]string, 1500)
	for i := range users {
		users[i] = fmt.Sprintf("u%d", i)
	}
	res, err := client.Messages.BatchSendAll(context.Background(), drim.BatchSendParams{From: "notice", ToUsers: users, ClientMsgID: "order-1", Type: "text", Body: drim.JSON(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1500 {
		t.Fatal(len(res.Results))
	}
	duplicates := 0
	for _, r := range res.Results {
		if r.Status == "duplicate" {
			duplicates++
		}
	}
	if duplicates != 600 {
		t.Fatalf("第二批重试后已发出的为 duplicate：%d", duplicates)
	}
	for _, call := range srv.Calls("messages.batch_send") {
		if call.JSON()["client_msg_id"] != "order-1" {
			t.Fatal("各批用同一个 client_msg_id")
		}
	}
}

func TestBatchAllStopsOnError(t *testing.T) {
	srv := newServer(t)
	var n atomic.Int32
	srv.Handle("friends.batch_add", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 2 {
			w.WriteHeader(403)
			w.Write(drimtest.ErrorBody("app_unavailable", "只读", nil))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Friends []struct {
				Username string `json:"username"`
			} `json:"friends"`
		}
		json.Unmarshal(body, &req)
		var added []string
		for _, f := range req.Friends {
			added = append(added, fmt.Sprintf(`{"username":%q}`, f.Username))
		}
		fmt.Fprintf(w, `{"added":[%s],"failed":[]}`, strings.Join(added, ","))
	})
	client := newClient(t, srv)
	items := make([]drim.AddFriendItem, 150)
	for i := range items {
		items[i] = drim.AddFriendItem{Username: fmt.Sprintf("f%d", i)}
	}
	res, err := client.Friends.BatchAddAll(context.Background(), "zhangsan", items)
	if e := asError(t, err); e.Code != drim.CodeAppUnavailable {
		t.Fatal(e)
	}
	if res == nil || len(res.Added) != 100 {
		t.Fatalf("返回已完成部分的结果：%v", res)
	}
}

func TestLocalRateLimit(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv, func(c *drim.Config) { c.RateLimit = &drim.RateLimit{PerSecond: 20, Burst: 1} })
	_, _ = client.Token(context.Background())
	started := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := client.Users.Get(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(started); d < 180*time.Millisecond {
		t.Fatalf("每秒 20 次：%v", d)
	}
}
