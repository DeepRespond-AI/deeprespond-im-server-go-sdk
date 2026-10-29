package drim_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

func TestChannelManagementTransport(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	raw, err := os.ReadFile("testdata/rtc-channel.json")
	if err != nil {
		t.Fatal(err)
	}
	srv.Respond("channels.create", 201, json.RawMessage(raw))
	params := drim.RTCChannelCreateParams{Name: "会议", Media: "video", Ext: json.RawMessage(`{"nested":{"number":1}}`)}
	result, err := client.Channels.Create(context.Background(), "Demo", params)
	if err != nil {
		t.Fatal(err)
	}
	if result.Channel.ChannelID != "Demo" || !result.Created || result.Channel.ExpiresAt != nil {
		t.Fatalf("强类型响应不一致: id=%q created=%v expires=%v", result.Channel.ChannelID, result.Created, result.Channel.ExpiresAt)
	}
	if got := srv.Calls("channels.create"); len(got) != 1 || got[0].JSON()["name"] != "会议" {
		t.Fatalf("请求: %+v", got)
	}
	operation := drim.RTCChannelRemoveMemberParams{Reason: "移出"}
	srv.Fail("channels.remove_member", "internal", drimtest.FailOptions{Status: 503, Times: 1})
	if _, err = client.Channels.RemoveMember(context.Background(), "Demo", "alice", operation); err != nil {
		t.Fatal(err)
	}
	calls := srv.Calls("channels.remove_member")
	if len(calls) != 2 {
		t.Fatalf("幂等重试 %d 次", len(calls))
	}
	key := calls[0].JSON()["operation_id"]
	if key == nil || key == "" || calls[1].JSON()["operation_id"] != key {
		t.Fatal("重试改变了管理操作键")
	}
	if operation.OperationID != "" {
		t.Fatal("修改了调用方参数")
	}
	srv.Fail("channels.issue_tickets", "internal", drimtest.FailOptions{Status: 503, Times: 1})
	_, err = client.Channels.IssueTickets(context.Background(), "Demo", drim.RTCChannelIssueTicketsParams{Users: []drim.ChannelsIssueTicketsUsers{{Username: "alice"}}})
	if err == nil || len(srv.Calls("channels.issue_tickets")) != 1 {
		t.Fatal("签发票据发生了自动重试")
	}
}

func TestChannelNullableResponsesAndExpiryThreeStates(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	raw, err := os.ReadFile("testdata/rtc-channel.json")
	if err != nil {
		t.Fatal(err)
	}
	srv.Respond("channels.update", 200, json.RawMessage(raw))
	expiration := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, field := range []drim.Field[time.Time]{{}, drim.Null[time.Time](), drim.Set(expiration)} {
		if _, err = client.Channels.Update(context.Background(), "Demo", drim.RTCChannelUpdateParams{Version: 1, ExpiresAt: field}); err != nil {
			t.Fatal(err)
		}
	}
	calls := srv.Calls("channels.update")
	if _, ok := calls[0].JSON()["expires_at"]; ok {
		t.Fatal("omitted expiry was sent")
	}
	if value, ok := calls[1].JSON()["expires_at"]; !ok || value != nil {
		t.Fatal("explicit null lost")
	}
	if value := calls[2].JSON()["expires_at"]; value == nil {
		t.Fatal("expiry value lost")
	}
	var ban drim.RTCChannelBanResult
	if err = json.Unmarshal([]byte(`{"ban":null}`), &ban); err != nil || ban.Ban != nil {
		t.Fatalf("null ban: %+v %v", ban, err)
	}
	var page drim.RTCChannelPage
	if err = json.Unmarshal([]byte(`{"items":[],"next_cursor":null}`), &page); err != nil || page.NextCursor != nil {
		t.Fatalf("null cursor: %+v %v", page, err)
	}
	var tickets drim.RTCChannelTicketsResult
	if err = json.Unmarshal([]byte(`{"tickets":[{"ticket":"private-ticket","username":"alice","role":"publisher","expires_at":"2026-10-10T00:00:00Z"}]}`), &tickets); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(tickets), "private-ticket") || strings.Contains(fmt.Sprint(tickets), "112 114 105 118 97 116 101") {
		t.Fatal("ticket leaked through formatting")
	}
}
