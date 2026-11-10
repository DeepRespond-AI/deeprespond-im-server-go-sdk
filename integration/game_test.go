//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

// gameRegion 是 harness 打开游戏房间时登记的大区（/env 的 game_region），没有打开时为空。
func requireGame(t *testing.T) {
	t.Helper()
	requireEnv(t)
	if env.GameRegion == "" {
		t.Skip("harness 没有打开游戏房间（在仓库根目录运行 harness，需要 deploy/game/dev_cluster_key）")
	}
}

// joinRoom 以玩家身份加入房间（客户端接口 C1，不连游戏节点）：登录拿 User Token，再用业务签的票据加入。
func joinRoom(t *testing.T, username, password, roomID, ticket string) map[string]any {
	t.Helper()
	post := func(path, token string, body any) (int, map[string]any) {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, env.APIURL+path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := direct.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, login := post("/client/v1/sessions", "", map[string]any{"app_key": env.OrgName + "#" + env.AppName, "grant_type": "password",
		"username": username, "password": password, "device_id": "phone-" + username, "platform": "ios"})
	check(t, status == http.StatusCreated, "登录 %s：%d %v", username, status, login)
	token, _ := login["access_token"].(string)
	status, joined := post("/client/v1/game/rooms/"+roomID+"/join", token, map[string]any{"client_session_id": "join-" + username, "ticket": ticket})
	check(t, status == http.StatusCreated, "加入 %s：%d %v", username, status, joined)
	return joined
}

// gameEvents 记录收到的 game_room.* 回调。
type gameEvents struct {
	mu      sync.Mutex
	secrets map[string][]string
	types   map[string][]string // 房间 ID → 事件类型
	closed  map[string]*callback.GameRoomClosed
	left    map[string]*callback.GameRoomMemberLeft
}

func (g *gameEvents) Secrets(_ context.Context, endpointID string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.secrets[endpointID], nil
}

func (g *gameEvents) add(roomID, typ string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.types[roomID] = append(g.types[roomID], typ)
}

func (g *gameEvents) seen(roomID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.types[roomID], ",")
}

func TestGameRooms(t *testing.T) {
	requireGame(t)
	im := newClient(t)
	ctx := context.Background()

	// 回调：订阅全部游戏事件，用 SDK 的处理器验签和解析。
	rcv := &gameEvents{secrets: map[string][]string{}, types: map[string][]string{}, closed: map[string]*callback.GameRoomClosed{},
		left: map[string]*callback.GameRoomMemberLeft{}}
	handler, err := callback.NewHandler(callback.Config{
		Secrets: rcv,
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Events: callback.EventHandlers{
			GameRoomCreated: func(_ context.Context, _ *callback.Event, d *callback.GameRoomCreated) error {
				rcv.add(d.RoomID, "created")
				return nil
			},
			GameRoomMemberJoined: func(_ context.Context, _ *callback.Event, d *callback.GameRoomMemberJoined) error {
				rcv.add(d.RoomID, "member_joined:"+d.Username)
				return nil
			},
			GameRoomMemberLeft: func(_ context.Context, _ *callback.Event, d *callback.GameRoomMemberLeft) error {
				rcv.mu.Lock()
				rcv.left[d.RoomID+"/"+d.Username] = d
				rcv.mu.Unlock()
				rcv.add(d.RoomID, "member_left:"+d.Username)
				return nil
			},
			GameRoomClosed: func(_ context.Context, _ *callback.Event, d *callback.GameRoomClosed) error {
				rcv.mu.Lock()
				rcv.closed[d.RoomID] = d
				rcv.mu.Unlock()
				rcv.add(d.RoomID, "closed")
				return nil
			},
		},
	})
	check(t, err == nil, "%v", err)
	server := httptest.NewServer(handler)
	defer server.Close()
	consoleCall(t, http.MethodGet, "/endpoints", nil)
	control(t, http.MethodPost, "/console/reauth", nil)
	created := consoleCall(t, http.MethodPost, "/endpoints", map[string]any{
		"name": uniq("sdk-go-game-"), "url": server.URL + "/im/callback", "batch_size": 10, "timeout_ms": 5000,
		"event_types": []string{"game_room.created", "game_room.started", "game_room.member_joined", "game_room.member_left", "game_room.closed"},
	})
	endpoint, ok := created["endpoint"].(map[string]any)
	check(t, ok, "新建回调地址失败：%v", created)
	endpointID := endpoint["endpoint_id"].(string)
	rcv.mu.Lock()
	rcv.secrets[endpointID] = []string{created["secret"].(string)}
	rcv.mu.Unlock()
	defer consoleCall(t, http.MethodDelete, "/endpoints/"+endpointID, nil)
	if endpoint["status"] == "unverified" {
		consoleCall(t, http.MethodPost, "/endpoints/"+endpointID+"/verify", map[string]any{})
	}

	// 大区。
	regions := must(im.Game.ListRegions(ctx))
	found := false
	for _, r := range regions {
		if r.Region == env.GameRegion {
			found = r.Available && r.Default
		}
	}
	check(t, found, "大区 %s 应可用且为默认：%+v", env.GameRegion, regions)

	// 建房：首次 201，同参数 200，参数不同 room_id_used。
	roomID := uniq("sdk-go-room-")
	params := drim.CreateGameRoomParams{Name: "SDK 联调", MaxPlayers: drim.Int64(2), StartMode: "manual", TickRate: drim.Int64(20),
		ReconnectWindowSeconds: drim.Int64(0), Ext: drim.JSON(`{"match":"m-1","n":2}`)}
	res := must(im.Game.CreateRoom(ctx, roomID, params))
	check(t, res.Created && res.Room.Status == "open" && res.Room.Region == env.GameRegion && res.Room.MaxPlayers == 2 &&
		res.Room.ReconnectWindowSeconds == 0 && res.Room.TickRate == 20, "建房：%+v", res.Room)
	check(t, sameJSON(res.Room.Ext, `{"match":"m-1","n":2}`), "ext：%s", res.Room.Ext)
	again := must(im.Game.CreateRoom(ctx, roomID, params))
	check(t, !again.Created && again.Room.CreatedAt.Equal(res.Room.CreatedAt), "重放应返回同一房间：%+v", again)
	params.MaxPlayers = drim.Int64(3)
	_, err = im.Game.CreateRoom(ctx, roomID, params)
	check(t, drim.IsReason(err, "conflict", "room_id_used"), "参数不同：%v", err)
	// 不给任何参数也能建房（发送 {}，不是空请求体）。
	plain := must(im.Game.CreateRoom(ctx, uniq("sdk-go-plain-"), drim.CreateGameRoomParams{}))
	check(t, plain.Created && plain.Room.MaxPlayers == 10 && plain.Room.StartMode == "auto", "默认参数：%+v", plain.Room)
	must(im.Game.CloseRoom(ctx, plain.Room.RoomID, drim.CloseGameRoomParams{}))

	// 查询和列表。
	room := must(im.Game.GetRoom(ctx, roomID))
	check(t, room.RoomID == roomID && room.Name == "SDK 联调", "GetRoom：%+v", room)
	_, err = im.Game.GetRoom(ctx, uniq("missing-"))
	check(t, drim.IsCode(err, "not_found"), "不存在的房间：%v", err)
	var listed int
	for r, err := range im.Game.AllRooms(ctx, drim.ListGameRoomsParams{Search: "sdk-go-room-", Status: "open,playing", ListOptions: drim.ListOptions{Limit: 1}}) {
		check(t, err == nil, "%v", err)
		if r.RoomID == roomID {
			listed++
		}
	}
	check(t, listed == 1, "列表应找到房间一次：%d", listed)

	// 两个玩家：签票据（指定座位）、以玩家身份加入。
	alice, bob := uniq("ga"), uniq("gb")
	for _, u := range []string{alice, bob} {
		must(im.Users.Create(ctx, drim.CreateUserParams{Username: u, Password: "Pw-" + u}))
	}
	tickets := must(im.Game.IssueTickets(ctx, roomID, drim.IssueGameTicketsParams{
		Users: []drim.GameTicketUser{{Username: alice, Seat: drim.Int64(1)}, {Username: strings.ToUpper(bob)}}, TTLSeconds: drim.Int64(120)}))
	check(t, len(tickets) == 2 && tickets[0].Username == alice && tickets[0].Seat != nil && *tickets[0].Seat == 1 &&
		tickets[1].Username == bob && tickets[1].Seat == nil && strings.HasPrefix(tickets[0].Ticket, "gtk_"), "票据：%+v", tickets)
	joined := joinRoom(t, alice, "Pw-"+alice, roomID, tickets[0].Ticket)
	check(t, joined["member"].(map[string]any)["seat"] == float64(1), "alice 应在座位 1：%v", joined)
	joinRoom(t, bob, "Pw-"+bob, roomID, tickets[1].Ticket)
	members := must(im.Game.ListMembers(ctx, roomID))
	check(t, len(members) == 2 && members[0].Status == "joined" && members[0].LeftAt == nil, "成员：%+v", members)
	check(t, must(im.Game.GetRoom(ctx, roomID)).PlayerCount == 2, "人数应为 2")

	// 手动开局：节点开始推进帧后变为 playing。
	must(im.Game.StartRoom(ctx, roomID))
	check(t, waitFor(10*time.Second, func() bool { return must(im.Game.GetRoom(ctx, roomID)).Status == "playing" }), "10 秒内应变为 playing")
	must(im.Game.StartRoom(ctx, roomID)) // 已在对局中：直接返回

	// 移出 bob：202、重放同一结果；不在房间里的用户 200。
	rm := must(im.Game.RemoveMember(ctx, roomID, bob, drim.RemoveGameMemberParams{Reason: "SDK 联调"}))
	check(t, rm.Removed && rm.Member != nil && rm.Member.Username == bob && rm.Member.LeaveReason == "removed" && len(rm.OperationID) == 32, "移出：%+v", rm)
	again2 := must(im.Game.RemoveMember(ctx, roomID, bob, drim.RemoveGameMemberParams{OperationID: rm.OperationID, Reason: "SDK 联调"}))
	check(t, again2.Member != nil && again2.Member.Seat == rm.Member.Seat, "同一 operation_id 应返回第一次的结果：%+v", again2)
	none := must(im.Game.RemoveMember(ctx, roomID, uniq("nobody"), drim.RemoveGameMemberParams{}))
	check(t, !none.Removed && none.Member == nil, "不在房间：%+v", none)

	// 结束：202 → closed，带结算结果；再结束为 200。
	closing := must(im.Game.CloseRoom(ctx, roomID, drim.CloseGameRoomParams{Reason: "比赛结束", Result: drim.JSON(`{"winner":"` + alice + `"}`)}))
	check(t, closing.Accepted && closing.Room.Status == "closing" && closing.Room.CloseReason == "closed_by_server", "结束：%+v", closing.Room)
	check(t, waitFor(15*time.Second, func() bool { return must(im.Game.GetRoom(ctx, roomID)).Status == "closed" }), "15 秒内应变为 closed")
	final := must(im.Game.GetRoom(ctx, roomID))
	check(t, final.ClosedAt != nil && strings.Contains(string(final.Result), alice), "结束后的房间：%+v", final)
	repeat := must(im.Game.CloseRoom(ctx, roomID, drim.CloseGameRoomParams{}))
	check(t, !repeat.Accepted && repeat.Room.Status == "closed", "再次结束：%+v", repeat.Room)
	_, err = im.Game.IssueTickets(ctx, roomID, drim.IssueGameTicketsParams{Users: []drim.GameTicketUser{{Username: alice}}})
	check(t, drim.IsReason(err, "conflict", "room_not_open"), "结束后签票据：%v", err)

	// 回调。
	check(t, waitFor(30*time.Second, func() bool { return strings.Contains(rcv.seen(roomID), "closed") }), "30 秒内应收到 game_room.closed：%s", rcv.seen(roomID))
	seen := rcv.seen(roomID)
	for _, want := range []string{"created", "member_joined:" + alice, "member_joined:" + bob, "member_left:" + bob} {
		check(t, strings.Contains(seen, want), "缺少回调 %s：%s", want, seen)
	}
	rcv.mu.Lock()
	closed, leftBob := rcv.closed[roomID], rcv.left[roomID+"/"+bob]
	rcv.mu.Unlock()
	check(t, leftBob != nil && leftBob.Reason == "removed" && leftBob.Origin != nil && *leftBob.Origin == "server", "bob 的离开回调：%+v", leftBob)
	check(t, closed.CloseReason == "closed_by_server" && closed.Origin != nil && *closed.Origin == "server" && len(closed.Members) == 2 &&
		strings.Contains(string(closed.Result), alice) && sameJSON(closed.Ext, `{"match":"m-1","n":2}`), "game_room.closed：%+v", closed)
}

// sameJSON 按内容比较 JSON（服务端存进数据库后键的顺序可能变化）。
func sameJSON(raw json.RawMessage, want string) bool {
	var a, b any
	return json.Unmarshal(raw, &a) == nil && json.Unmarshal([]byte(want), &b) == nil && reflect.DeepEqual(a, b)
}
