//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func TestGroups(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	owner, m1, m2, m3 := uniq("g"), uniq("g"), uniq("g"), uniq("g")
	for _, n := range []string{owner, m1, m2, m3} {
		must(im.Users.Create(ctx, drim.CreateUserParams{Username: n}))
	}
	created := must(im.Groups.Create(ctx, drim.CreateGroupParams{
		Owner: owner, Name: "项目组", Attributes: map[string]string{"biz": "order-1"}, Members: []string{m1, "nobody-" + owner},
	}))
	gid := created.GroupID
	check(t, created.Owner != nil && *created.Owner == owner && created.MemberCount == 2, "%+v", created)
	check(t, created.Results[0].Status == "joined" && created.Results[1].Error != nil && created.Results[1].Error.Code == "not_found", "%+v", created.Results)
	check(t, len(created.FailedItems()) == 1, "FailedItems")
	group := must(im.Groups.Get(ctx, gid))
	check(t, group.Name == "项目组" && group.Attributes["biz"] == "order-1", "%+v", group)
	changed := must(im.Groups.Update(ctx, gid, drim.UpdateGroupParams{Name: drim.String("新名字"), MaxMembers: drim.Null[int](), Version: drim.Int64(group.InfoVersion)}))
	check(t, changed.Changed && changed.Name == "新名字", "%+v", changed)
	_, err := im.Groups.Update(ctx, gid, drim.UpdateGroupParams{Name: drim.String("x"), Version: drim.Int64(group.InfoVersion)})
	check(t, drim.IsCode(err, drim.CodeVersionConflict), "%v", err)
	added := must(im.Groups.AddMembers(ctx, gid, []string{m2, m3, m1}))
	statuses := map[string]string{}
	for _, r := range added.Results {
		statuses[r.Username] = r.Status
	}
	check(t, statuses[m2] == "joined" && statuses[m3] == "joined" && statuses[m1] == "already_member", "%v", statuses)
	members := must(im.Groups.Members(ctx, gid, drim.ListMembersParams{}))
	check(t, members.Count == 4 && len(members.Items) == 4, "%+v", members)
	check(t, must(im.Groups.Members(ctx, gid, drim.ListMembersParams{KnownVersion: drim.Int64(members.Version)})).NotModified, "not_modified")
	check(t, must(im.Groups.Member(ctx, gid, m1)).Role == "member", "角色")
	nick := must(im.Groups.UpdateMember(ctx, gid, m1, drim.UpdateMemberParams{GroupNickname: drim.String("小一"), Attributes: drim.Set(map[string]drim.Field[string]{"t": drim.Set("1")})}))
	check(t, nick.Member.GroupNickname == "小一", "%+v", nick)
	must(im.Groups.SetRole(ctx, gid, m1, "admin"))
	check(t, must(im.Groups.Member(ctx, gid, m1)).Role == "admin", "设为管理员")
	must(im.Groups.MuteMember(ctx, gid, m2, drim.Int64(600)))
	check(t, must(im.Groups.Member(ctx, gid, m2)).Muted, "禁言")
	must(im.Groups.UnmuteMember(ctx, gid, m2))
	check(t, must(im.Groups.SetMuteAll(ctx, gid, true)).MuteAll, "全员禁言")
	check(t, !must(im.Groups.SetMuteAll(ctx, gid, false)).MuteAll, "解除全员禁言")
	must(im.Groups.AddToBlacklist(ctx, gid, m3, "广告"))
	var blacklisted []string
	for e, err := range im.Groups.AllBlacklist(ctx, gid, drim.ListOptions{}) {
		blacklisted = append(blacklisted, must(e, err).Username)
	}
	check(t, slices.Equal(blacklisted, []string{m3}), "%v", blacklisted)
	_, err = im.Groups.Member(ctx, gid, m3)
	check(t, drim.IsCode(err, drim.CodeNotGroupMember), "拉黑同时移出群：%v", err)
	must(im.Groups.RemoveFromBlacklist(ctx, gid, m3))
	removed := must(im.Groups.RemoveMembers(ctx, gid, []string{m2, m3}))
	check(t, removed.Results[0].Status == "removed" && removed.Results[1].Status == "not_member", "%+v", removed.Results)
	for r, err := range im.Groups.AllRequests(ctx, gid, drim.ListGroupRequestsParams{}) {
		t.Fatalf("没有申请：%v %v", r, err)
	}
	var mine []drim.UserGroup
	for g, err := range im.Groups.AllOfUser(ctx, m1, drim.ListOptions{}) {
		mine = append(mine, must(g, err))
	}
	check(t, len(mine) == 1 && mine[0].GroupID == gid && mine[0].Self.Role == "admin", "%+v", mine)
	settings := must(im.Groups.SetInviteSettings(ctx, m1, drim.Set("need_confirm")))
	check(t, settings.InviteMode != nil && *settings.InviteMode == "need_confirm", "%+v", settings)
	check(t, must(im.Groups.SetInviteSettings(ctx, m1, drim.Null[string]())).InviteMode == nil, "跟随应用")
	check(t, must(im.Groups.InviteSettings(ctx, m1)).EffectiveInviteMode != "", "生效的设置")
	transferred := must(im.Groups.TransferOwner(ctx, gid, m1))
	check(t, transferred.Owner != nil && *transferred.Owner == m1, "%+v", transferred)
	check(t, must(im.Groups.Disable(ctx, gid, "违规")).Status == "disabled", "封禁")
	check(t, must(im.Groups.Enable(ctx, gid)).Status == "active", "解封")
	found := false
	for g, err := range im.Groups.All(ctx, drim.ListGroupsParams{Owner: m1}) {
		found = found || must(g, err).GroupID == gid
	}
	check(t, found, "按群主列出")
	check(t, im.Groups.Delete(ctx, gid, "") == nil && im.Groups.Delete(ctx, gid, "") == nil, "重复解散为 204")
	check(t, must(im.Groups.Get(ctx, gid)).Status == "dismissed", "已解散")
	for _, n := range []string{owner, m1, m2, m3} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestMessagesAndConversations(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	a, b := uniq("m"), uniq("m")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: a}))
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: b}))
	key := uniq("order-")
	raw := `{"price":1.00,"name":"中文 <b>&"}`
	first := must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "custom", Body: drim.JSON(raw), ClientMsgID: key}))
	check(t, !first.Duplicate && first.Seq == 1 && first.ClientMsgID == key && string(first.Body) == raw, "%+v %s", first, first.Body)
	again := must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "custom", Body: drim.JSON(`{"other":true}`), ClientMsgID: key}))
	check(t, again.Duplicate && again.MessageID == first.MessageID, "%+v", again)
	text := must(im.Messages.Send(ctx, drim.SendMessageParams{From: b, ToUser: a, Type: "text", Body: drim.JSON(`{"text":"你好"}`)}))
	check(t, len(text.ClientMsgID) == 32, "SDK 生成的 client_msg_id：%q", text.ClientMsgID)
	online := must(im.Messages.Send(ctx, drim.SendMessageParams{From: a, ToUser: b, Type: "custom", Body: drim.JSON(`{"typing":true}`), OnlineOnly: true}))
	check(t, online.OnlineOnly && !online.Duplicate, "%+v", online)
	conversation := must(im.Conversations.Single(ctx, a, b))
	cid := conversation.ConversationID
	check(t, conversation.MaxSeq == 2, "%+v", conversation)
	check(t, must(im.Conversations.Get(ctx, cid)).ConversationID == cid, "会话")
	message := must(im.Messages.Get(ctx, first.MessageID))
	check(t, string(message.Body) == raw, "原始 JSON 逐字节相同：%s", message.Body)
	edited := must(im.Messages.Edit(ctx, text.MessageID, drim.EditMessageParams{Body: drim.JSON(`{"text":"改过的"}`), Ext: drim.Set(drim.JSON(`{"edited_by":"server"}`))}))
	check(t, edited.Changed && edited.Message.Edited != nil, "%+v", edited)
	check(t, must(im.Messages.Pin(ctx, text.MessageID, "")).Changed, "置顶")
	var pins []string
	for m, err := range im.Conversations.AllPins(ctx, cid) {
		pins = append(pins, must(m, err).MessageID)
	}
	check(t, slices.Equal(pins, []string{text.MessageID}), "%v", pins)
	must(im.Messages.Unpin(ctx, text.MessageID, "测试"))
	seqPage := must(im.Conversations.Messages(ctx, cid, drim.SeqParams{AfterSeq: drim.Int64(0), Limit: 10}))
	check(t, len(seqPage.Items) == 2 && seqPage.Items[0].Seq == 1 && !seqPage.HasMore, "%+v", seqPage)
	var newestFirst []int64
	for m, err := range im.Conversations.AllMessages(ctx, cid, drim.SeqParams{Limit: 1}) {
		newestFirst = append(newestFirst, must(m, err).Seq)
	}
	check(t, slices.Equal(newestFirst, []int64{2, 1}), "%v", newestFirst)
	state := must(im.Conversations.StateOfUser(ctx, b, cid))
	check(t, state.UnreadCount >= 1, "%+v", state)
	check(t, must(im.Conversations.UnreadCount(ctx, b)).Total >= 1, "未读数")
	read := must(im.Conversations.MarkRead(ctx, b, cid, nil))
	check(t, read.Conversation.UnreadCount == 0, "%+v", read)
	check(t, must(im.Conversations.MarkAllRead(ctx, b)).Count == 0, "全部已读")
	var conversations []string
	for c, err := range im.Conversations.AllOfUser(ctx, a, drim.ListOptions{}) {
		conversations = append(conversations, must(c, err).ConversationID)
	}
	check(t, slices.Equal(conversations, []string{cid}), "%v", conversations)
	check(t, must(im.Conversations.DeleteMessages(ctx, a, cid, []int64{1})).Changed, "删除消息")
	check(t, must(im.Conversations.Clear(ctx, a, cid)).Changed, "清空")
	check(t, must(im.Conversations.Delete(ctx, a, cid, true)).Changed, "删除会话")
	recalled := must(im.Messages.Recall(ctx, first.MessageID, "测试"))
	check(t, recalled.Changed && recalled.Message.Recalled != nil, "%+v", recalled)
	batch := must(im.Messages.BatchRecall(ctx, []string{text.MessageID, "999999"}, ""))
	check(t, batch.Results[0].Changed && batch.Results[0].Error == nil && batch.Results[1].Error != nil && batch.Results[1].Error.Code == "not_found", "%+v", batch.Results)
	sent := must(im.Messages.BatchSendAll(ctx, drim.BatchSendParams{From: b, ToUsers: []string{a, "nobody-" + a}, ClientMsgID: uniq("notice-"), Type: "custom", Body: drim.JSON(`{"n":1}`)}))
	check(t, sent.Results[0].Status == "sent" && sent.Results[1].Error != nil && sent.Results[1].Error.Code == "not_found", "%+v", sent.Results)
	foundSent := false
	for m, err := range im.Messages.AllSentBy(ctx, a, drim.SentByParams{}) {
		foundSent = foundSent || must(m, err).MessageID == first.MessageID
	}
	check(t, foundSent, "发送的消息")
	exported := must(im.Messages.Export(ctx, drim.ExportParams{Limit: 1}))
	check(t, len(exported.Items) <= 1, "导出")
	_, err := im.Messages.Get(ctx, "1")
	check(t, drim.IsCode(err, drim.CodeNotFound), "%v", err)
	for _, n := range []string{a, b} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestGroupMessage(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	owner, member := uniq("gm"), uniq("gm")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: owner}))
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: member}))
	group := must(im.Groups.Create(ctx, drim.CreateGroupParams{Owner: owner, Members: []string{member}}))
	sent := must(im.Messages.Send(ctx, drim.SendMessageParams{From: owner, ToGroup: group.GroupID, Type: "text",
		Body: drim.JSON(`{"text":"@所有人"}`), Mentions: &drim.Mentions{Usernames: []string{member}, All: true}}))
	check(t, sent.ConversationID == group.GroupID && sent.Mentions != nil && sent.Mentions.All, "%+v", sent)
	system := must(im.Messages.Send(ctx, drim.SendMessageParams{ToGroup: group.GroupID, Type: "text", Body: drim.JSON(`{"text":"系统通知"}`)}))
	check(t, system.Sender == nil && system.SenderType == "system", "%+v", system)
	var body map[string]string
	_ = json.Unmarshal(system.Body, &body)
	check(t, body["text"] == "系统通知", "%v", body)
	_ = im.Groups.Delete(ctx, group.GroupID, "")
	for _, n := range []string{owner, member} {
		_ = im.Users.Delete(ctx, n)
	}
}
