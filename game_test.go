package drim_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

// gameRoomJSON 是服务端返回的一个房间（字段与 internal/modules/gameroom 的 RoomView 一致）。
const gameRoomJSON = `{"room_id":"rank-8812","name":"排位赛","status":"open","region":"cn-east","region_name":"华东","max_players":4,
"player_count":0,"sync_mode":"frame","tick_rate":30,"start_mode":"manual","access_mode":"ticket","max_duration_seconds":1800,
"reconnect_window_seconds":0,"snapshot":{"enabled":true,"interval_seconds":30},"ext":{"match_id":"m-1","n":1.0},"version":1,
"created_at":"2026-11-09T08:00:00.000Z","started_at":null,"closed_at":null,"close_reason":"","result":null}`

func roomBody(room string) json.RawMessage { return json.RawMessage(`{"room":` + room + `}`) }

func TestGameCreateRoom(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	ctx := context.Background()
	srv.Respond("game.create_room", 201, roomBody(gameRoomJSON))
	params := drim.CreateGameRoomParams{
		Name: "排位赛", Region: "cn-east", MaxPlayers: drim.Int64(4), StartMode: "manual",
		ReconnectWindowSeconds: drim.Int64(0), // 0 有意义（不等待重连），必须发出
		Snapshot:               &drim.GameRoomSnapshotParams{Enabled: true},
		Ext:                    drim.JSON(`{"match_id":"m-1","n":1.0}`),
	}
	res, err := client.Game.CreateRoom(ctx, "rank-8812", params)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Room.RoomID != "rank-8812" || res.Room.Status != "open" || res.Room.StartedAt != nil || res.Room.Result != nil {
		t.Fatalf("结果：%+v", res)
	}
	if string(res.Room.Ext) != `{"match_id":"m-1","n":1.0}` || !res.Room.Snapshot.Enabled || res.Room.Snapshot.IntervalSeconds != 30 {
		t.Fatalf("ext 应逐字节保留：%s %+v", res.Room.Ext, res.Room.Snapshot)
	}
	call := srv.Calls("game.create_room")[0]
	if call.Method != "PUT" || call.Path != "/game/rooms/rank-8812" {
		t.Fatalf("请求：%s %s", call.Method, call.Path)
	}
	body := call.JSON()
	if body["reconnect_window_seconds"] != float64(0) || body["max_players"] != float64(4) || body["start_mode"] != "manual" {
		t.Fatalf("请求体：%s", call.Body)
	}
	if _, ok := body["tick_rate"]; ok {
		t.Fatalf("没给的参数不应发送：%s", call.Body)
	}
	if snap, _ := body["snapshot"].(map[string]any); snap["enabled"] != true {
		t.Fatalf("snapshot：%s", call.Body)
	}

	// 同参数重放：200，Created 为假；网络错误后自动重试（建房本身幂等）。
	srv.Respond("game.create_room", 200, roomBody(gameRoomJSON))
	srv.Fail("game.create_room", "internal", drimtest.FailOptions{Status: 503})
	res, err = client.Game.CreateRoom(ctx, "rank-8812", drim.CreateGameRoomParams{})
	if err != nil || res.Created {
		t.Fatalf("重放：%v %+v", err, res)
	}
	if n := len(srv.Calls("game.create_room")); n != 3 {
		t.Fatalf("应自动重试一次，共 %d 次", n)
	}
	if last := srv.Calls("game.create_room")[2]; string(last.Body) != "{}" {
		t.Fatalf("没有参数时发送空对象：%q", last.Body)
	}

	// 参数不同：conflict + details.reason。
	srv.Fail("game.create_room", "conflict", drimtest.FailOptions{Details: map[string]any{"reason": "room_id_used"}})
	_, err = client.Game.CreateRoom(ctx, "rank-8812", drim.CreateGameRoomParams{MaxPlayers: drim.Int64(5)})
	var apiErr *drim.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "conflict" || apiErr.Reason() != "room_id_used" {
		t.Fatalf("应为 room_id_used：%v", err)
	}
}

func TestGameQueries(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	ctx := context.Background()
	srv.Respond("game.get_room", 200, roomBody(gameRoomJSON))
	room, err := client.Game.GetRoom(ctx, "rank-8812")
	if err != nil || room.RoomID != "rank-8812" || room.RegionName != "华东" || room.ReconnectWindowSeconds != 0 {
		t.Fatalf("GetRoom：%v %+v", err, room)
	}
	srv.Fail("game.get_room", "not_found", drimtest.FailOptions{})
	if room, err = client.Game.GetRoom(ctx, "missing"); room != nil || !drim.IsCode(err, "not_found") {
		t.Fatalf("不存在：%v %v", room, err)
	}

	srv.Respond("game.list_regions", 200, `{"items":[{"region":"cn-east","name":"华东","default":true,"available":true}]}`)
	regions, err := client.Game.ListRegions(ctx)
	if err != nil || len(regions) != 1 || !regions[0].Default || !regions[0].Available {
		t.Fatalf("ListRegions：%v %+v", err, regions)
	}

	srv.Respond("game.list_members", 200, `{"items":[{"seat":0,"username":"alice","status":"connected","transport":"udp","rtt_ms":23,
		"joined_at":"2026-11-09T08:00:10.000Z","connected_ms":5000,"left_at":null,"leave_reason":""},
		{"seat":1,"username":"bob","status":"left","transport":"","rtt_ms":null,"joined_at":"2026-11-09T08:00:11.000Z","connected_ms":0,
		"left_at":"2026-11-09T08:01:11.000Z","leave_reason":"connect_timeout"}]}`)
	members, err := client.Game.ListMembers(ctx, "rank-8812")
	if err != nil || len(members) != 2 || members[0].RttMs == nil || *members[0].RttMs != 23 || members[1].RttMs != nil ||
		members[1].LeftAt == nil || members[1].LeaveReason != "connect_timeout" {
		t.Fatalf("ListMembers：%v %+v", err, members)
	}

	// 分页：按 next_cursor 翻页，带上筛选条件。
	srv.Respond("game.list_rooms", 200, `{"items":[`+gameRoomJSON+`],"next_cursor":null}`, drimtest.Times(1))
	srv.Respond("game.list_rooms", 200, `{"items":[`+gameRoomJSON+`],"next_cursor":"c2"}`, drimtest.Times(1))
	var n int
	for room, err := range client.Game.AllRooms(ctx, drim.ListGameRoomsParams{Status: "open,playing", Search: "rank-", ListOptions: drim.ListOptions{Limit: 1}}) {
		if err != nil {
			t.Fatal(err)
		}
		if room.RoomID != "rank-8812" {
			t.Fatal(room.RoomID)
		}
		n++
	}
	calls := srv.Calls("game.list_rooms")
	if n != 2 || len(calls) != 2 || calls[0].Query.Get("status") != "open,playing" || calls[0].Query.Get("search") != "rank-" ||
		calls[0].Query.Get("limit") != "1" || calls[0].Query.Get("cursor") != "" || calls[1].Query.Get("cursor") != "c2" {
		t.Fatalf("翻页：%d 项，请求 %+v", n, calls)
	}
}

func TestGameTicketsNotRetried(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	ctx := context.Background()
	srv.Respond("game.issue_tickets", 201, `{"tickets":[{"username":"alice","seat":0,"ticket":"gtk_secret","expires_at":"2026-11-09T08:02:00.000Z"},
		{"username":"bob","seat":null,"ticket":"gtk_secret2","expires_at":"2026-11-09T08:02:00.000Z"}]}`)
	tickets, err := client.Game.IssueTickets(ctx, "rank-8812", drim.IssueGameTicketsParams{
		Users: []drim.GameTicketUser{{Username: "alice", Seat: drim.Int64(0)}, {Username: "Bob"}}, TTLSeconds: drim.Int64(60)})
	if err != nil || len(tickets) != 2 || tickets[0].Seat == nil || *tickets[0].Seat != 0 || tickets[1].Seat != nil || tickets[1].Ticket != "gtk_secret2" {
		t.Fatalf("IssueTickets：%v %+v", err, tickets)
	}
	body := srv.Calls("game.issue_tickets")[0].JSON()
	users := body["users"].([]any)
	if users[0].(map[string]any)["seat"] != float64(0) || users[1].(map[string]any)["seat"] != nil || body["ttl_seconds"] != float64(60) {
		t.Fatalf("请求体：%v", body)
	}
	// 签票据会作废旧票据，SDK 不自动重试，由调用方决定是否重新签发。
	srv.Fail("game.issue_tickets", "internal", drimtest.FailOptions{Status: 503})
	if _, err := client.Game.IssueTickets(ctx, "rank-8812", drim.IssueGameTicketsParams{Users: []drim.GameTicketUser{{Username: "alice"}}}); err == nil {
		t.Fatal("应返回错误")
	}
	if n := len(srv.Calls("game.issue_tickets")); n != 2 {
		t.Fatalf("不应自动重试：%d 次", n)
	}
	// 本地检查必填项。
	if _, err := client.Game.IssueTickets(ctx, "rank-8812", drim.IssueGameTicketsParams{Users: []drim.GameTicketUser{{}}}); err == nil {
		t.Fatal("用户名为空应在本地报错")
	}
}

func TestGameStartCloseRemove(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	ctx := context.Background()

	srv.Respond("game.start_room", 200, roomBody(gameRoomJSON))
	srv.Fail("game.start_room", "internal", drimtest.FailOptions{Status: 503})
	room, err := client.Game.StartRoom(ctx, "rank-8812")
	if err != nil || room.RoomID != "rank-8812" || len(srv.Calls("game.start_room")) != 2 {
		t.Fatalf("StartRoom（幂等，自动重试）：%v", err)
	}
	if c := srv.Calls("game.start_room")[0]; c.Method != "POST" || len(c.Body) != 0 {
		t.Fatalf("开局不带请求体：%s %q", c.Method, c.Body)
	}

	closing := `{"room_id":"rank-8812","name":"","status":"closing","region":"cn-east","region_name":"cn-east","max_players":4,"player_count":2,
		"sync_mode":"frame","tick_rate":30,"start_mode":"auto","access_mode":"ticket","max_duration_seconds":3600,"reconnect_window_seconds":60,
		"snapshot":{"enabled":false,"interval_seconds":0},"ext":{},"version":5,"created_at":"2026-11-09T08:00:00.000Z",
		"started_at":"2026-11-09T08:00:15.000Z","closed_at":null,"close_reason":"closed_by_server","result":{"winner":"alice"}}`
	srv.Respond("game.close_room", 202, roomBody(closing), drimtest.Times(1))
	res, err := client.Game.CloseRoom(ctx, "rank-8812", drim.CloseGameRoomParams{Reason: "比赛结束", Result: drim.JSON(`{"winner":"alice"}`)})
	if err != nil || !res.Accepted || res.Room.Status != "closing" || string(res.Room.Result) != `{"winner":"alice"}` {
		t.Fatalf("CloseRoom：%v %+v", err, res)
	}
	srv.Respond("game.close_room", 200, roomBody(closing))
	if res, err = client.Game.CloseRoom(ctx, "rank-8812", drim.CloseGameRoomParams{}); err != nil || res.Accepted {
		t.Fatalf("已在结束：%v %+v", err, res)
	}
	if body := srv.Calls("game.close_room")[0].JSON(); body["reason"] != "比赛结束" || body["result"].(map[string]any)["winner"] != "alice" {
		t.Fatalf("请求体：%v", body)
	}

	// 移出玩家：SDK 生成 operation_id，重试时不变，不改调用方的参数。
	removed := `{"operation_id":"%s","member":{"seat":1,"username":"bob","status":"left","transport":"udp","rtt_ms":null,
		"joined_at":"2026-11-09T08:00:11.000Z","connected_ms":9000,"left_at":"2026-11-09T08:05:00.000Z","leave_reason":"removed"}}`
	srv.Handle("game.remove_member", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = fmt.Fprintf(w, removed, body["operation_id"])
	})
	srv.Fail("game.remove_member", "internal", drimtest.FailOptions{Status: 503})
	params := drim.RemoveGameMemberParams{Reason: "作弊"}
	rm, err := client.Game.RemoveMember(ctx, "rank-8812", "bob", params)
	if err != nil || !rm.Removed || rm.Member == nil || rm.Member.LeaveReason != "removed" {
		t.Fatalf("RemoveMember：%v %+v", err, rm)
	}
	calls := srv.Calls("game.remove_member")
	key, _ := calls[0].JSON()["operation_id"].(string)
	if len(calls) != 2 || len(key) != 32 || calls[1].JSON()["operation_id"] != key || rm.OperationID != key || params.OperationID != "" {
		t.Fatalf("operation_id：%d 次，%q / %v / %q", len(calls), key, calls[1].JSON()["operation_id"], rm.OperationID)
	}
	if calls[0].Path != "/game/rooms/rank-8812/members/bob/remove" {
		t.Fatal(calls[0].Path)
	}
	// 用户不在房间里：200，member 为 null。
	srv.Respond("game.remove_member", 200, `{"operation_id":"op-1","member":null}`)
	rm, err = client.Game.RemoveMember(ctx, "rank-8812", "carol", drim.RemoveGameMemberParams{OperationID: "op-1"})
	if err != nil || rm.Removed || rm.Member != nil || srv.Calls("game.remove_member")[2].JSON()["operation_id"] != "op-1" {
		t.Fatalf("不在房间：%v %+v", err, rm)
	}
}

func TestGameCallbackEvents(t *testing.T) {
	var closed *callback.GameRoomClosed
	var left *callback.GameRoomMemberLeft
	h, secret := newGameHandler(t, callback.EventHandlers{
		GameRoomClosed: func(ctx context.Context, ev *callback.Event, d *callback.GameRoomClosed) error {
			closed = d
			return nil
		},
		GameRoomMemberLeft: func(ctx context.Context, ev *callback.Event, d *callback.GameRoomMemberLeft) error {
			left = d
			return nil
		},
	})
	for _, name := range []string{"game_room.closed", "game_room.member_left"} {
		body, err := os.ReadFile(filepath.Join("testdata", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		req := drimtest.SignedRequest("1840200000000000007", body, secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("%s：%d %s", name, rec.Code, rec.Body)
		}
	}
	if closed == nil || closed.RoomID != "rank-8812" || closed.CloseReason != "closed_by_server" || closed.DurationMs != 1847000 ||
		len(closed.Members) != 2 || closed.Members[0].BillableMinutes != 31 || closed.StartedAt == nil {
		t.Fatalf("game_room.closed：%+v", closed)
	}
	if string(closed.Result) != `{"winner":"alice","scores":{"alice":12,"bob":9}}` || string(closed.Ext) != `{"match_id":"m-20261109-8812"}` {
		t.Fatalf("result、ext 应保留原文：%s %s", closed.Result, closed.Ext)
	}
	if left == nil || left.Reason != "leave" || left.ConnectedMs != 182000 || left.LeftAt == nil {
		t.Fatalf("game_room.member_left：%+v", left)
	}
}

// newGameHandler 创建一个只处理游戏事件的回调处理器，返回它和签名用的密钥。
func newGameHandler(t *testing.T, events callback.EventHandlers) (*callback.Handler, string) {
	t.Helper()
	secret := "cbs_" + strings.Repeat("g", 43)
	h, err := callback.NewHandler(callback.Config{
		Secrets: callback.StaticSecrets{"1840200000000000007": {secret}},
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Events:  events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, secret
}
