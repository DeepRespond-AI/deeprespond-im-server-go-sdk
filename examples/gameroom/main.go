// 示例：游戏的业务服务端在匹配成功后建房、给每个玩家签加入票据（App 用客户端 SDK 凭票据加入），对局结束时结算并结束房间，
// 再从 game_room.closed 回调拿到每个玩家的在线时长（《游戏通道/03-房间管理》《05-结束回调与计费》的后端接口设计）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

func main() {
	client, err := drim.New(drim.Config{
		BaseURL:      os.Getenv("IM_BASE_URL"),
		OrgName:      os.Getenv("IM_ORG_NAME"),
		AppName:      os.Getenv("IM_APP_NAME"),
		ClientID:     os.Getenv("IM_CLIENT_ID"),
		ClientSecret: os.Getenv("IM_CLIENT_SECRET"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// 匹配服务凑齐一局后调用：建房并给每个玩家签一张票据，票据随匹配结果下发给各自的 App。
	http.HandleFunc("POST /api/matches", func(w http.ResponseWriter, r *http.Request) {
		var match struct {
			MatchID string   `json:"match_id"`
			Players []string `json:"players"`
		}
		if err := json.NewDecoder(r.Body).Decode(&match); err != nil || len(match.Players) == 0 {
			http.Error(w, "参数错误", http.StatusBadRequest)
			return
		}
		ext, _ := json.Marshal(map[string]string{"match_id": match.MatchID})
		// 房间 ID 用匹配 ID：同参数重复建房返回同一房间，匹配服务超时重试不会建出两个房间
		room, err := client.Game.CreateRoom(r.Context(), match.MatchID, drim.CreateGameRoomParams{
			MaxPlayers: drim.Int64(int64(len(match.Players))),
			StartMode:  "all_ready", // 所有人准备后开局
			TickRate:   drim.Int64(30),
			Ext:        ext,
		})
		if drim.IsReason(err, drim.CodeUnavailable, "game_capacity_unavailable") {
			http.Error(w, "游戏服务繁忙，请稍后重试", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			log.Printf("建房失败：%v", err)
			http.Error(w, "暂时不可用", http.StatusServiceUnavailable)
			return
		}
		users := make([]drim.GameTicketUser, len(match.Players))
		for i, p := range match.Players {
			users[i] = drim.GameTicketUser{Username: p, Seat: drim.Int64(int64(i))}
		}
		tickets, err := client.Game.IssueTickets(r.Context(), room.Room.RoomID, drim.IssueGameTicketsParams{Users: users})
		if err != nil {
			log.Printf("签发加入票据失败：%v", err) // 错误中不含票据
			http.Error(w, "暂时不可用", http.StatusServiceUnavailable)
			return
		}
		out := map[string]string{}
		for _, t := range tickets {
			out[t.Username] = t.Ticket
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"room_id": room.Room.RoomID, "tickets": out})
	})

	// 游戏逻辑判定胜负后调用：带结算结果结束房间，玩家的 App 收到结束通知和结果。
	http.HandleFunc("POST /api/matches/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		var result json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			http.Error(w, "参数错误", http.StatusBadRequest)
			return
		}
		res, err := client.Game.CloseRoom(r.Context(), r.PathValue("id"), drim.CloseGameRoomParams{Reason: "对局结束", Result: result})
		if drim.IsCode(err, drim.CodeNotFound) {
			http.Error(w, "房间不存在", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("结束房间失败：%v", err)
			http.Error(w, "暂时不可用", http.StatusServiceUnavailable)
			return
		}
		if !res.Accepted {
			log.Printf("房间 %s 之前已在结束（%s）", res.Room.RoomID, res.Room.CloseReason)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// 回调：房间结束（包括到期、空房间、节点故障）时记录每个玩家的在线时长。
	handler, err := callback.NewHandler(callback.Config{
		Secrets: callback.StaticSecrets{os.Getenv("IM_CALLBACK_ENDPOINT_ID"): {os.Getenv("IM_CALLBACK_SECRET")}},
		// 单个实例用内存存储；多实例时换成共享的存储（如 Redis），见 README 的回调一节
		Nonces: callback.NewMemoryNonceStore(),
		Dedup:  callback.NewMemoryDedupStore(0),
		Events: callback.EventHandlers{
			GameRoomClosed: func(ctx context.Context, ev *callback.Event, d *callback.GameRoomClosed) error {
				for _, m := range d.Members {
					log.Printf("房间 %s（%s）：%s 在线 %s，计 %d 分钟", d.RoomID, d.CloseReason, m.Username,
						time.Duration(m.ConnectedMs)*time.Millisecond, m.BillableMinutes)
				}
				return nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("POST /im/callback", handler)

	if err := http.ListenAndServe(":8080", nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
