package drim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

func newServer(t *testing.T) *drimtest.Server {
	t.Helper()
	srv := drimtest.NewServer()
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, srv *drimtest.Server, mutate ...func(*drim.Config)) *drim.Client {
	t.Helper()
	cfg := srv.Config()
	for _, m := range mutate {
		m(&cfg)
	}
	client, err := drim.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func asError(t *testing.T, err error) *drim.Error {
	t.Helper()
	var e *drim.Error
	if !errors.As(err, &e) {
		t.Fatalf("期望 *drim.Error，得到 %T: %v", err, err)
	}
	return e
}

// logBuffer 记录日志，便于检查告警。
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

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestConfigValidation(t *testing.T) { // S-44
	base := drim.Config{OrgName: "o", AppName: "a", ClientID: "id", ClientSecret: "secret"}
	cases := []struct {
		url string
		ok  bool
	}{
		{"http://im.example.com", false},
		{"http://127.0.0.1:8080/", true},
		{"https://im.example.com/im?x=1", false},
		{"https://im.example.com/gw/", true},
		{"http://192.168.1.10", true},
		{"http://10.1.2.3:80", true},
		{"http://172.20.0.1", true},
		{"http://172.32.0.1", false},
		{"http://[::1]:9000", true},
		{"ftp://im.example.com", false},
		{"https://im.example.com#frag", false},
		{"im.example.com", false},
	}
	for _, c := range cases {
		cfg := base
		cfg.BaseURL = c.url
		client, err := drim.New(cfg)
		if (err == nil) != c.ok {
			t.Errorf("%s：期望 ok=%v，得到 %v", c.url, c.ok, err)
		}
		if client != nil {
			client.Close()
		}
	}
	cfg := base
	cfg.BaseURL = "https://im.example.com"
	cfg.ClientSecret = ""
	if _, err := drim.New(cfg); err == nil {
		t.Error("缺少 ClientSecret 应当报错")
	}
	cfg.ClientSecret = "x"
	cfg.Timeout = -time.Second
	if _, err := drim.New(cfg); err == nil {
		t.Error("负数的时长应当报错")
	}
}

func TestBaseURLTrailingSlashAndPrefix(t *testing.T) { // S-44
	srv := newServer(t)
	client := newClient(t, srv, func(c *drim.Config) { c.BaseURL += "/" })
	if _, err := client.Users.Get(context.Background(), "zhangsan"); err != nil {
		t.Fatal(err)
	}
	calls := srv.Calls("users.get")
	if len(calls) != 1 || calls[0].Path != "/users/zhangsan" {
		t.Fatalf("请求的路径不对：%+v", calls)
	}
	if got := calls[0].Header.Get("User-Agent"); !strings.HasPrefix(got, "deeprespond-im-server-sdk/go/") {
		t.Errorf("User-Agent：%s", got)
	}
	if got := calls[0].Header.Get("X-Request-ID"); len(got) != 26 {
		t.Errorf("X-Request-ID 应为 26 个字符：%q", got)
	}
}

func TestDecodeKeepsRawAndUnknownFields(t *testing.T) { // S-18
	srv := newServer(t)
	srv.Respond("users.get", 200, `{"username":"zhangsan","nickname":"张三","status":"future_status","version":3,`+
		`"created_at":"2026-10-06T08:00:00.123Z","attributes":{"vip":"1"},"new_field":{"a":[1,2]}}`)
	client := newClient(t, srv)
	user, err := client.Users.Get(context.Background(), "zhangsan")
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != "future_status" || user.Version != 3 || user.Attributes["vip"] != "1" {
		t.Fatalf("解码不对：%+v", user)
	}
	if user.CreatedAt.UnixMilli()%1000 != 123 {
		t.Errorf("时间：%v", user.CreatedAt)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(user.Raw, &raw); err != nil || string(raw["new_field"]) != `{"a":[1,2]}` {
		t.Errorf("Raw 中应当有新字段：%s", user.Raw)
	}
}

func TestUpdateUserThreeState(t *testing.T) { // S-15
	srv := newServer(t)
	client := newClient(t, srv)
	_, err := client.Users.Update(context.Background(), "zhangsan", drim.UpdateUserParams{
		Attributes: drim.Set(map[string]drim.Field[string]{"level": drim.Null[string](), "vip": drim.Set("1"), "skip": {}}),
		Version:    drim.Int64(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(srv.Calls("users.update")[0].Body)
	if body != `{"attributes":{"level":null,"vip":"1"},"version":7}` {
		t.Fatalf("请求体：%s", body)
	}
	_, _ = client.Users.Update(context.Background(), "zhangsan", drim.UpdateUserParams{
		Nickname: drim.String(""), Attributes: drim.Null[map[string]drim.Field[string]](),
	})
	if body := string(srv.Calls("users.update")[1].Body); body != `{"nickname":"","attributes":null}` {
		t.Fatalf("请求体：%s", body)
	}
}

func TestRequiredFieldLocalCheck(t *testing.T) { // S-16
	srv := newServer(t)
	client := newClient(t, srv)
	_, err := client.Friends.SetSettings(context.Background(), "zhangsan", drim.Field[string]{})
	e := asError(t, err)
	if e.Code != drim.CodeInvalidParams || e.Details["field"] != "friend_add_mode" || e.Op != "friends.set_settings" {
		t.Fatalf("错误：%+v", e)
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("本地检查不通过时不应发请求")
	}
	if _, err := client.Friends.SetSettings(context.Background(), "zhangsan", drim.Null[string]()); err != nil {
		t.Fatal(err)
	}
	if body := string(srv.Calls("friends.set_settings")[0].Body); body != `{"friend_add_mode":null}` {
		t.Fatalf("请求体：%s", body)
	}
	// 嵌套的必填项：批量创建中第 2 个缺少用户名
	_, err = client.Users.BatchCreate(context.Background(), []drim.CreateUserParams{{Username: "a"}, {Nickname: "x"}})
	if e := asError(t, err); e.Details["field"] != "users[1].username" {
		t.Fatalf("字段路径：%+v", e)
	}
	// 路径参数
	_, err = client.Users.Get(context.Background(), "")
	if e := asError(t, err); e.Code != drim.CodeInvalidParams {
		t.Fatal(e)
	}
	_, err = client.Users.Get(context.Background(), "..")
	if e := asError(t, err); e.Code != drim.CodeInvalidParams {
		t.Fatal(e)
	}
}

func TestEmptyBody(t *testing.T) { // S-17
	srv := newServer(t)
	var lengths []string
	var mu sync.Mutex
	srv.Handle("users.disable", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lengths = append(lengths, fmt.Sprintf("%d/%d/%v", r.ContentLength, len(body), r.TransferEncoding))
		mu.Unlock()
		w.Write([]byte(`{"username":"zhangsan","status":"disabled"}`))
	})
	client := newClient(t, srv)
	if _, err := client.Users.Disable(context.Background(), "zhangsan", drim.DisableUserParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Users.Disable(context.Background(), "zhangsan", drim.DisableUserParams{Reason: "spam"}); err != nil {
		t.Fatal(err)
	}
	if lengths[0] != "0/0/[]" {
		t.Fatalf("没有参数时应发送长度为 0 的请求体：%s", lengths[0])
	}
	if body := string(srv.Calls("users.disable")[1].Body); body != `{"reason":"spam"}` {
		t.Fatal(body)
	}
}

func TestRawBodyEncoding(t *testing.T) { // S-60
	srv := newServer(t)
	client := newClient(t, srv)
	_, err := client.Messages.Send(context.Background(), drim.SendMessageParams{
		From: "notice", ToUser: "zhangsan", ClientMsgID: "order-1", Type: "custom",
		Body: drim.JSON(`{ "html": "<a href=\"x\">&", "text": "中文", "price": 1.0, "u": "<" }`),
		Push: &drim.MessagePushOptions{Title: "<标题>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(srv.Calls("messages.send")[0].Body)
	want := `{"from":"notice","to_user":"zhangsan","client_msg_id":"order-1","type":"custom",` +
		`"body":{"html":"<a href=\"x\">&","text":"中文","price":1.0,"u":"<"},"push":{"title":"<标题>"}}`
	if body != want {
		t.Fatalf("请求体\n得到 %s\n期望 %s", body, want)
	}
	// Body 不是 JSON 对象：本地拒绝
	_, err = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(`[1]`)})
	if e := asError(t, err); e.Code != drim.CodeInvalidParams || e.Details["field"] != "body" {
		t.Fatalf("%+v", e)
	}
	// ToUser、ToGroup 恰好一个
	_, err = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", Type: "text", Body: drim.JSON(`{}`)})
	if e := asError(t, err); e.Code != drim.CodeInvalidParams {
		t.Fatal(e)
	}
	// client_msg_id 的格式
	_, err = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text",
		Body: drim.JSON(`{}`), ClientMsgID: "evt_1"})
	if e := asError(t, err); e.Code != drim.CodeInvalidParams || e.Details["field"] != "client_msg_id" {
		t.Fatal(e)
	}
	marshaled, _ := drim.Marshal(map[string]string{"a": "<b>&"})
	if string(marshaled) != `{"a":"<b>&"}` {
		t.Fatal(string(marshaled))
	}
}

func TestSendGeneratesClientMsgID(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	_, _ = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(`{"text":"x"}`)})
	_, _ = client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "custom", Body: drim.JSON(`{}`), OnlineOnly: true})
	calls := srv.Calls("messages.send")
	id, _ := calls[0].JSON()["client_msg_id"].(string)
	if len(id) != 32 {
		t.Fatalf("应当生成 32 个十六进制字符的 client_msg_id：%q", id)
	}
	if _, ok := calls[1].JSON()["client_msg_id"]; ok {
		t.Fatal("只推在线的消息不去重，不生成 client_msg_id")
	}
}

func TestPayloadTooLarge(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	big := `{"text":"` + strings.Repeat("a", 1<<20) + `"}`
	_, err := client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(big)})
	if e := asError(t, err); e.Code != drim.CodePayloadTooLarge || e.StatusCode != 0 {
		t.Fatal(e)
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("不应发出")
	}
}

func TestLookupLimit(t *testing.T) { // S-62
	srv := newServer(t)
	client := newClient(t, srv)
	names := make([]string, 101)
	for i := range names {
		names[i] = fmt.Sprintf("u%d", i%99)
	}
	_, err := client.Users.Lookup(context.Background(), names)
	if e := asError(t, err); e.Code != drim.CodeInvalidParams || e.Details["field"] != "usernames" {
		t.Fatal(e)
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("不应发出")
	}
}

func TestErrorDecoding(t *testing.T) {
	srv := newServer(t)
	srv.Fail("users.create", "already_exists", drimtest.FailOptions{Message: "用户名已被占用", Details: map[string]any{"field": "username"}})
	client := newClient(t, srv)
	_, err := client.Users.Create(context.Background(), drim.CreateUserParams{Username: "zhangsan"})
	e := asError(t, err)
	if e.StatusCode != 409 || e.Code != "already_exists" || e.RequestID != "FAKEREQUESTID" || e.Op != "users.create" || e.Attempts != 1 || e.Retried {
		t.Fatalf("%+v", e)
	}
	if !drim.IsCode(err, drim.CodeAlreadyExists) || drim.IsTemporary(err) {
		t.Fatal("分类不对")
	}
	if !strings.Contains(e.Error(), "users.create: 409 already_exists: 用户名已被占用") || !strings.Contains(e.Error(), "field=username") {
		t.Fatal(e.Error())
	}
	srv.Fail("users.get", "not_found", drimtest.FailOptions{Details: map[string]any{"reason": "user_deleted"}})
	_, err = client.Users.Get(context.Background(), "x")
	if !drim.IsReason(err, "not_found", "user_deleted") {
		t.Fatal(err)
	}
	srv.Fail("users.get", "tenant_unavailable", drimtest.FailOptions{Details: map[string]any{"reason": "arrears"}})
	_, err = client.Users.Get(context.Background(), "x")
	if !drim.IsServiceUnavailable(err) {
		t.Fatal(err)
	}
}

func TestBatchResultHelpers(t *testing.T) {
	srv := newServer(t)
	srv.Respond("groups.add_members", 200, `{"changed":true,"member_version":5,"results":[{"username":"a","status":"joined"},`+
		`{"username":"b","code":"not_found","message":"用户不存在","details":null},{"username":"c","code":"limit_exceeded","details":{"reason":"user_group_limit"}}]}`)
	srv.Respond("chatrooms.set_attributes", 200, `{"changed":true,"attributes_version":3,"failed":{"k2":{"code":"limit_exceeded","details":{"reason":"attribute_limit"}}}}`)
	srv.Respond("moderation.add_words", 200, `{"added":1,"duplicated":["x"],"invalid":[{"word":"a","reason":"too_short"}]}`)
	client := newClient(t, srv)
	res, err := client.Groups.AddMembers(context.Background(), "100", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	failed := res.FailedItems()
	if res.OK() || len(failed) != 2 || failed[0].Key != "b" || failed[1].Reason() != "user_group_limit" || res.Results[0].Error != nil {
		t.Fatalf("%+v", failed)
	}
	var be *drim.BatchError
	if !errors.As(res.Err(), &be) || len(be.Failed) != 2 {
		t.Fatal(res.Err())
	}
	attrs, err := client.Chatrooms.SetAttributes(context.Background(), "1", map[string]string{"k1": "v", "k2": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if items := attrs.FailedItems(); len(items) != 1 || items[0].Key != "k2" || items[0].Code != "limit_exceeded" {
		t.Fatalf("%+v", items)
	}
	words, err := client.Moderation.AddWords(context.Background(), "1", []string{"a", "x", "加微信"})
	if err != nil {
		t.Fatal(err)
	}
	if items := words.FailedItems(); len(items) != 1 || items[0].Key != "a" || items[0].Code != "too_short" {
		t.Fatalf("%+v", items)
	}
}

func TestPagination(t *testing.T) { // S-19、S-59
	srv := newServer(t)
	pages := map[string]string{
		"":   `{"items":[{"username":"a"}],"next_cursor":"c1"}`,
		"c1": `{"items":[],"next_cursor":"c2"}`,
		"c2": `{"items":[{"username":"b"}],"next_cursor":null}`,
	}
	srv.Handle("users.list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(pages[r.URL.Query().Get("cursor")]))
	})
	calls := map[string]string{
		"":  `{"items":[],"next_cursor":"x"}`,
		"x": `{"items":[{"call_id":"9"}],"next_cursor":""}`,
	}
	srv.Handle("calls.list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(calls[r.URL.Query().Get("cursor")]))
	})
	client := newClient(t, srv)
	var names []string
	for user, err := range client.Users.All(context.Background(), drim.ListUsersParams{Status: "active"}) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, user.Username)
	}
	if strings.Join(names, ",") != "a,b" || len(srv.Calls("users.list")) != 3 {
		t.Fatalf("%v %d", names, len(srv.Calls("users.list")))
	}
	if q := srv.Calls("users.list")[0].Query; q.Get("status") != "active" || q.Has("cursor") {
		t.Fatalf("查询参数：%v", q)
	}
	var ids []string
	for call, err := range client.Calls.All(context.Background(), drim.ListCallsParams{}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, call.CallID)
	}
	if strings.Join(ids, ",") != "9" || len(srv.Calls("calls.list")) != 2 {
		t.Fatalf("空字符串的 next_cursor 视为结束：%v", ids)
	}
	page, err := client.Calls.List(context.Background(), drim.ListCallsParams{ListOptions: drim.ListOptions{Cursor: "x"}})
	if err != nil || page.NextCursor != "" || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	// 提前结束遍历
	for range client.Users.All(context.Background(), drim.ListUsersParams{}) {
		break
	}
	// 出错时产出错误后结束
	srv.Fail("users.list", "invalid_argument", drimtest.FailOptions{Details: map[string]any{"field": "cursor"}})
	count := 0
	for _, err := range client.Users.All(context.Background(), drim.ListUsersParams{}) {
		count++
		if !drim.IsCode(err, drim.CodeInvalidArgument) {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}

func TestExportAndSeqPagination(t *testing.T) { // S-59
	srv := newServer(t)
	srv.Handle("messages.export", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("after_message_id") {
		case "":
			w.Write([]byte(`{"items":[{"message_id":"1"},{"message_id":"2"}],"next_after_message_id":"2","has_more":true}`))
		default:
			w.Write([]byte(`{"items":[],"next_after_message_id":null,"has_more":false}`))
		}
	})
	srv.Handle("conversations.messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("before_seq") {
		case "":
			w.Write([]byte(`{"items":[{"seq":9},{"seq":10}],"next_before_seq":9,"has_more":true}`))
		case "9":
			w.Write([]byte(`{"items":[],"next_before_seq":5,"has_more":true}`))
		default:
			w.Write([]byte(`{"items":[{"seq":3},{"seq":4}],"next_before_seq":3,"has_more":false}`))
		}
	})
	client := newClient(t, srv)
	var ids []string
	for m, err := range client.Messages.ExportAll(context.Background(), drim.ExportParams{Limit: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.MessageID)
	}
	if strings.Join(ids, ",") != "1,2" || len(srv.Calls("messages.export")) != 2 {
		t.Fatalf("条数等于 limit 时多请求一页：%v", ids)
	}
	var seqs []int64
	for m, err := range client.Conversations.AllMessages(context.Background(), "c1", drim.SeqParams{}) {
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, m.Seq)
	}
	if fmt.Sprint(seqs) != "[10 9 4 3]" {
		t.Fatalf("从最新向旧的方向：%v", seqs)
	}
	_, err := client.Conversations.Messages(context.Background(), "c1", drim.SeqParams{AfterSeq: drim.Int64(1), BeforeSeq: drim.Int64(5)})
	if e := asError(t, err); e.Code != drim.CodeInvalidParams {
		t.Fatal(e)
	}
}

func TestVersionedPage(t *testing.T) {
	srv := newServer(t)
	srv.Respond("friends.list", 200, `{"friend_version":57,"not_modified":true,"friend_count":3,"max_count":3000,"items":[],"next_cursor":null}`)
	srv.Respond("groups.members", 200, `{"member_version":8,"not_modified":false,"member_count":2,"items":[{"username":"a"},{"username":"b"}],"next_cursor":null}`)
	client := newClient(t, srv)
	page, err := client.Friends.List(context.Background(), "zhangsan", drim.ListFriendsParams{KnownVersion: drim.Int64(57)})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Calls("friends.list")[0].Query.Get("known_version") != "57" || !page.NotModified {
		t.Fatalf("%+v", page)
	}
	members, err := client.Groups.Members(context.Background(), "1", drim.ListMembersParams{Muted: drim.Bool(false)})
	if err != nil {
		t.Fatal(err)
	}
	if members.Version != 8 || members.Count != 2 || len(members.Items) != 2 || srv.Calls("groups.members")[0].Query.Get("muted") != "false" {
		t.Fatalf("%+v", members)
	}
}

func TestBatchCreateAll(t *testing.T) { // S-20
	srv := newServer(t)
	existing := map[string]bool{"u10": true, "u150": true, "u249": true}
	srv.Handle("users.batch_create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Users []struct {
				Username string `json:"username"`
			} `json:"users"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		created, failed := []string{}, []string{}
		for _, u := range req.Users {
			if existing[u.Username] {
				failed = append(failed, `{"username":"`+u.Username+`","code":"already_exists","message":"用户名已被占用"}`)
			} else {
				created = append(created, `{"username":"`+u.Username+`"}`)
			}
		}
		fmt.Fprintf(w, `{"created":[%s],"failed":[%s]}`, strings.Join(created, ","), strings.Join(failed, ","))
	})
	client := newClient(t, srv)
	users := make([]drim.CreateUserParams, 250)
	for i := range users {
		users[i] = drim.CreateUserParams{Username: fmt.Sprintf("u%d", i)}
	}
	res, err := client.Users.BatchCreateAll(context.Background(), users)
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Calls("users.batch_create")) != 3 || len(res.Created) != 247 || len(res.Failed) != 3 {
		t.Fatalf("%d %d %d", len(srv.Calls("users.batch_create")), len(res.Created), len(res.Failed))
	}
	if res.Failed[0].Key != "u10" || res.Failed[0].Details != nil || res.OK() {
		t.Fatalf("%+v", res.Failed[0])
	}
	if !bytes.HasPrefix(res.Raw, []byte(`{"batches":[`)) {
		t.Fatal(string(res.Raw[:20]))
	}
}

func TestTextCheckUnified(t *testing.T) { // S-61
	srv := newServer(t)
	srv.Handle("moderation.check_text", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"fields"`)) {
			w.Write([]byte(`{"fields":{"text":{"action":"pass","text":"a","hits":[]},"nickname":{"action":"reject","text":"b","hits":[]}}}`))
			return
		}
		w.Write([]byte(`{"action":"reject","text":"加微信","hits":[{"word":"加微信","action":"reject"}]}`))
	})
	client := newClient(t, srv)
	one, err := client.Moderation.CheckText(context.Background(), drim.CheckTextParams{Scene: "message", Text: "加微信"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Fields["text"].Action != "reject" || len(one.Fields) != 1 {
		t.Fatalf("%+v", one)
	}
	many, err := client.Moderation.CheckText(context.Background(), drim.CheckTextParams{Scene: "message", Text: "a",
		Fields: map[string]string{"nickname": "b", "remark": "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if many.Fields["nickname"].Action != "reject" || many.Fields["text"].Action != "pass" {
		t.Fatalf("%+v", many)
	}
}

func TestStatusFlags(t *testing.T) { // S-58
	srv := newServer(t)
	srv.Respond("messages.send", 200, `{"message_id":"1","seq":5}`)
	srv.Respond("chatrooms.send", 200, `{"room_id":"r","message_id":"9","client_msg_id":"k","sender":"x","type":"text",`+
		`"body":{"text":"这次的"},"priority":"normal","created_at":"2026-10-06T00:00:00.000Z"}`)
	client := newClient(t, srv)
	res, err := client.Messages.Send(context.Background(), drim.SendMessageParams{From: "a", ToUser: "b", Type: "text", Body: drim.JSON(`{}`)})
	if err != nil || !res.Duplicate {
		t.Fatal(res, err)
	}
	room, err := client.Chatrooms.Send(context.Background(), "r", drim.SendRoomMessageParams{ClientMsgID: "k", Type: "text", Body: drim.JSON(`{"text":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !room.Duplicate || room.MessageID == nil || *room.MessageID != "9" || room.Body != nil || room.Sender != nil || room.Type != "" || room.CreatedAt == nil {
		t.Fatalf("重复提交的其他字段应为零值：%+v", room)
	}
	srv.Respond("chatrooms.send", 201, `{"room_id":"r","message_id":"10","type":"text","body":{"text":"x"}}`)
	room, _ = client.Chatrooms.Send(context.Background(), "r", drim.SendRoomMessageParams{Type: "text", Body: drim.JSON(`{"text":"x"}`)})
	if room.Duplicate || string(room.Body) != `{"text":"x"}` {
		t.Fatalf("%+v", room)
	}
	if id, _ := srv.Calls("chatrooms.send")[1].JSON()["client_msg_id"].(string); len(id) != 32 {
		t.Fatal("普通优先级应当生成 client_msg_id")
	}
	_, _ = client.Chatrooms.Send(context.Background(), "r", drim.SendRoomMessageParams{Type: "text", Body: drim.JSON(`{}`), Priority: "low"})
	if _, ok := srv.Calls("chatrooms.send")[2].JSON()["client_msg_id"]; ok {
		t.Fatal("低优先级不生成 client_msg_id")
	}
}

func TestSensitiveString(t *testing.T) {
	ticket := drim.LoginTicket{Ticket: "ult_secret", ExpiresIn: 300}
	if text := fmt.Sprintf("%v %+v", ticket, ticket); strings.Contains(text, "ult_secret") {
		t.Fatal(text)
	}
}

func TestQueryEncoding(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	at := time.Date(2026, 10, 6, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	_, _ = client.Messages.SentBy(context.Background(), "zhang san", drim.SentByParams{StartTime: at})
	call := srv.Calls("messages.sent_by")[0]
	if call.Path != "/users/zhang%20san/sent-messages" || call.Query.Get("start_time") != "2026-10-06T00:00:00Z" {
		t.Fatalf("%s %v", call.Path, call.Query)
	}
	_, _ = client.Chatrooms.Attributes(context.Background(), "r", "a", "b,c")
	if q := srv.Calls("chatrooms.attributes")[0].Query.Get("keys"); q != "a,b,c" {
		t.Fatal(q)
	}
	_, _ = client.Moderation.Items(context.Background(), drim.ListItemsParams{Status: []string{"pending", "reviewing"}})
	if q := srv.Calls("moderation.items")[0].Query["status"]; len(q) != 2 {
		t.Fatal(q)
	}
}

func TestCloseAndStats(t *testing.T) { // S-45
	srv := newServer(t)
	client := newClient(t, srv)
	if _, err := client.Users.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	stats := client.Stats().Ops["users.get"]
	if stats.Requests != 1 || stats.Failures != 0 {
		t.Fatalf("%+v", stats)
	}
	if client.AppID() != "99579846509723648" {
		t.Fatal(client.AppID())
	}
	client.Close()
	before := len(srv.Requests())
	_, err := client.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeClientUnavailable || e.Details["cause"] != "closed" {
		t.Fatal(e)
	}
	if len(srv.Requests()) != before {
		t.Fatal("关闭后不应发请求")
	}
}

func TestInvalidResponses(t *testing.T) { // S-46
	srv := newServer(t)
	huge := bytes.Repeat([]byte("a"), 16<<20+10)
	srv.Handle("users.get", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"username":"`))
		w.Write(huge)
	}, drimtest.Times(1))
	attempts := 0
	srv.Handle("groups.get", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(502)
			w.Write([]byte("<html><body>Bad Gateway</body></html>"))
			return
		}
		w.Write([]byte(`{"group_id":"1"}`))
	})
	client := newClient(t, srv)
	_, err := client.Users.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeInvalidResponse {
		t.Fatal(e)
	}
	group, err := client.Groups.Get(context.Background(), "1")
	if err != nil || group.GroupID != "1" || attempts != 2 {
		t.Fatal(group, err, attempts)
	}
	srv.Respond("groups.get", 502, "<html>bad</html>")
	_, err = client.Groups.Get(context.Background(), "1")
	e := asError(t, err)
	if e.Code != drim.CodeInvalidResponse || e.StatusCode != 502 || !drim.IsTemporary(err) || e.Attempts != 4 {
		t.Fatalf("%+v", e)
	}
	if !strings.Contains(e.Details["body_prefix"].(string), "bad") {
		t.Fatal(e.Details)
	}
	srv.Respond("presence.get", 200, "not json")
	_, err = client.Presence.Get(context.Background(), "a")
	if e := asError(t, err); e.Code != drim.CodeInvalidResponse || e.StatusCode != 200 {
		t.Fatal(e)
	}
}
