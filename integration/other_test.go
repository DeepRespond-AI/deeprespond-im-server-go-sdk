//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"slices"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func TestMedia(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	owner := uniq("md")
	data := make([]byte, 3000)
	rand.Read(data)
	big := make([]byte, 33<<20+123)
	rand.Read(big)
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: owner}))
	file := must(im.Media.Upload(ctx, bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "file", Name: "小文件.bin", Owner: owner}))
	check(t, file.Status == "active" && file.Size == 3000 && file.URL != "", "%+v", file)
	var progress []int64
	multipart := must(im.Media.Upload(ctx, bytes.NewReader(big), int64(len(big)), drim.UploadParams{Purpose: "attachment", Kind: "file", Name: "big.bin",
		OnProgress: func(done, total int64) { progress = append(progress, done) }}))
	check(t, multipart.Size == int64(len(big)) && progress[len(progress)-1] == int64(len(big)) && len(progress) == 3, "%+v %v", multipart, progress)
	info := must(im.Media.File(ctx, file.FileID))
	check(t, info.Name != nil && *info.Name == "小文件.bin" && info.Owner != nil && *info.Owner == owner, "%+v", info)
	items := must(im.Media.DownloadURLs(ctx, []string{file.URL, file.URL + "x"}))
	check(t, items[0].DownloadURL != "" && items[0].Error == nil && items[1].Error != nil, "%+v", items)
	var buf bytes.Buffer
	n := must(im.Media.Download(ctx, file.URL, &buf))
	check(t, n == 3000 && bytes.Equal(buf.Bytes(), data), "下载 %d", n)
	session := must(im.Media.CreateUpload(ctx, drim.CreateUploadParams{Purpose: "attachment", Kind: "file", Size: 40 << 20, Name: "x.bin"}))
	check(t, session.UploadMethod == "multipart" && session.PartCount == 3, "%+v", session)
	parts := must(im.Media.PartURLs(ctx, session.FileID, []int{1, 2}))
	check(t, len(parts) == 2 && parts[0].PartNumber == 1 && parts[0].Headers["Content-Length"] != "", "%+v", parts)
	check(t, len(must(im.Media.UploadedParts(ctx, session.FileID))) == 0, "没有已上传的分片")
	check(t, im.Media.CancelUpload(ctx, session.FileID) == nil && im.Media.CancelUpload(ctx, session.FileID) == nil, "重复取消为 204")
	group := must(im.Groups.Create(ctx, drim.CreateGroupParams{Owner: owner}))
	shared := must(im.Media.Upload(ctx, bytes.NewReader([]byte("group file")), 10, drim.UploadParams{Purpose: "group_file", Kind: "file", Name: "g.txt", GroupID: group.GroupID}))
	page := must(im.Media.GroupFiles(ctx, group.GroupID, drim.ListOptions{}))
	check(t, page.FileCount == 1 && len(page.Items) == 1 && page.Items[0].FileID == shared.FileID, "%+v", page)
	check(t, must(im.Media.Usage(ctx)).UsedBytes >= 3000, "用量")
	check(t, im.Media.DeleteFile(ctx, file.FileID) == nil && im.Media.DeleteFile(ctx, file.FileID) == nil, "重复删除为 204")
	_ = im.Groups.Delete(ctx, group.GroupID, "")
	_ = im.Users.Delete(ctx, owner)
}

func TestPush(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	a, b := uniq("p"), uniq("p")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: a}))
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: b}))
	settings := must(im.Push.Settings(ctx, a))
	check(t, settings.DND == nil && settings.SettingsVersion == 0, "%+v", settings)
	changed := must(im.Push.UpdateSettings(ctx, a, drim.UpdatePushSettingsParams{
		DND:        drim.Set(drim.DND{DurationSeconds: drim.Int64(3600)}),
		QuietHours: drim.Set(drim.QuietHours{Start: "23:00", End: "07:00", Timezone: "Asia/Shanghai"}),
		Preview:    drim.Set("sender_only"),
	}))
	check(t, changed.Changed && changed.DND != nil && changed.DND.Until != nil && changed.Preview != nil && *changed.Preview == "sender_only", "%+v", changed)
	off := must(im.Push.UpdateSettings(ctx, a, drim.UpdatePushSettingsParams{DND: drim.Null[drim.DND](), QuietHours: drim.Null[drim.QuietHours](), Preview: drim.Null[string]()}))
	check(t, off.DND == nil && off.Preview == nil, "%+v", off)
	mute := must(im.Push.MuteSingle(ctx, a, b, drim.MuteConversationParams{Mode: "none", DurationSeconds: drim.Int64(600)}))
	check(t, mute.Changed && mute.Target == b, "%+v", mute)
	group := must(im.Groups.Create(ctx, drim.CreateGroupParams{Owner: a, Members: []string{b}}))
	must(im.Push.MuteGroup(ctx, b, group.GroupID, drim.MuteConversationParams{Mode: "mention_only"}))
	var mutes []string
	for m, err := range im.Push.AllMutes(ctx, a, drim.ListPushMutesParams{}) {
		m = must(m, err)
		mutes = append(mutes, m.ConversationType+":"+m.Target)
	}
	check(t, slices.Equal(mutes, []string{"single:" + b}), "%v", mutes)
	listed := must(im.Push.Mutes(ctx, b, drim.ListPushMutesParams{}))
	check(t, listed.NextCursor == "" && len(listed.Items) == 1 && listed.Items[0].Mode == "mention_only", "%+v", listed)
	check(t, must(im.Push.Mutes(ctx, b, drim.ListPushMutesParams{KnownVersion: drim.Int64(listed.Version)})).NotModified, "not_modified")
	check(t, must(im.Push.UnmuteSingle(ctx, a, b)).Changed && must(im.Push.UnmuteGroup(ctx, b, group.GroupID)).Changed, "解除")
	check(t, len(must(im.Push.Devices(ctx, a))) == 0, "没有设备")
	check(t, drim.IsCode(im.Push.UnbindDevice(ctx, a, "123"), drim.CodeNotFound), "解绑不存在的设备")
	_ = im.Groups.Delete(ctx, group.GroupID, "")
	for _, n := range []string{a, b} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestChatrooms(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	owner, admin, user := uniq("r"), uniq("r"), uniq("r")
	for _, n := range []string{owner, admin, user} {
		must(im.Users.Create(ctx, drim.CreateUserParams{Username: n}))
	}
	room := must(im.Chatrooms.Create(ctx, drim.CreateChatroomParams{Name: uniq("直播间"), Owner: owner, Attributes: map[string]string{"topic": "新品"}}))
	rid := room.RoomID
	check(t, room.Owner != nil && *room.Owner == owner && room.AttributeCount == 1, "%+v", room)
	updated := must(im.Chatrooms.Update(ctx, rid, drim.UpdateChatroomParams{Description: drim.String("介绍"), MaxMembers: drim.Null[int](), Version: drim.Int64(room.InfoVersion)}))
	check(t, updated.Changed && updated.Description == "介绍", "%+v", updated)
	found := false
	for r, err := range im.Chatrooms.All(ctx, drim.ListChatroomsParams{Owner: owner}) {
		found = found || must(r, err).RoomID == rid
	}
	check(t, found, "按所有者列出")
	check(t, must(im.Chatrooms.SetMuteAll(ctx, rid, true)).MuteAll, "全员禁言")
	must(im.Chatrooms.SetMuteAll(ctx, rid, false))
	check(t, must(im.Chatrooms.SetAdmin(ctx, rid, admin)).Changed, "设为管理员")
	must(im.Chatrooms.RemoveAdmin(ctx, rid, admin))
	counts := must(im.Chatrooms.OnlineCounts(ctx, []string{rid, "1"}))
	check(t, len(counts) == 1 && counts[0].RoomID == rid && counts[0].MemberCount == 0, "%+v", counts)
	for m, err := range im.Chatrooms.AllMembers(ctx, rid, drim.ListOptions{}) {
		t.Fatalf("没有成员：%v %v", m, err)
	}
	check(t, !must(im.Chatrooms.Member(ctx, rid, user)).InRoom, "不在聊天室中")
	kicked := must(im.Chatrooms.Kick(ctx, rid, []string{user}))
	check(t, kicked.Results[0].Result == "not_in_room", "%+v", kicked)
	muted := must(im.Chatrooms.Mute(ctx, rid, []string{user, owner}, drim.RoomPenaltyParams{DurationSeconds: drim.Int64(600), Reason: "刷屏"}))
	check(t, muted.Results[0].Result == "muted" && muted.Results[1].Error != nil && muted.Results[1].Error.Reason() == "owner_protected", "%+v", muted.Results)
	var mutes []string
	for m, err := range im.Chatrooms.AllMutes(ctx, rid, drim.ListOptions{}) {
		mutes = append(mutes, must(m, err).Username)
	}
	check(t, slices.Equal(mutes, []string{user}), "%v", mutes)
	must(im.Chatrooms.Unmute(ctx, rid, []string{user}))
	must(im.Chatrooms.Ban(ctx, rid, []string{user}, drim.RoomPenaltyParams{}))
	bans := must(im.Chatrooms.Bans(ctx, rid, drim.ListOptions{}))
	check(t, len(bans.Items) == 1 && bans.Items[0].Username == user, "%+v", bans)
	must(im.Chatrooms.Unban(ctx, rid, []string{user}))
	must(im.Chatrooms.AddToAllowlist(ctx, rid, []string{user}))
	var allow []string
	for e, err := range im.Chatrooms.AllAllowlist(ctx, rid, drim.ListOptions{}) {
		allow = append(allow, must(e, err).Username)
	}
	check(t, slices.Equal(allow, []string{user}), "%v", allow)
	must(im.Chatrooms.RemoveFromAllowlist(ctx, rid, []string{user}))
	attrs := must(im.Chatrooms.SetAttributes(ctx, rid, map[string]string{"score": "1:0", "stage": "上半场"}))
	check(t, attrs.Changed && attrs.OK(), "%+v", attrs)
	got := must(im.Chatrooms.Attributes(ctx, rid, "score"))
	check(t, got.Attributes["score"].Value == "1:0" && len(got.Attributes) == 1, "%+v", got)
	must(im.Chatrooms.RemoveAttributes(ctx, rid, []string{"stage"}))
	key := uniq("k")
	sent := must(im.Chatrooms.Send(ctx, rid, drim.SendRoomMessageParams{From: owner, Type: "custom", Body: drim.JSON(`{"gift":"rocket","n":1.0}`), ClientMsgID: key}))
	check(t, !sent.Duplicate && sent.MessageID != nil && string(sent.Body) == `{"gift":"rocket","n":1.0}`, "%+v", sent)
	dup := must(im.Chatrooms.Send(ctx, rid, drim.SendRoomMessageParams{From: owner, Type: "custom", Body: drim.JSON(`{"gift":"other"}`), ClientMsgID: key}))
	check(t, dup.Duplicate && *dup.MessageID == *sent.MessageID && dup.Body == nil, "%+v", dup)
	low := must(im.Chatrooms.Send(ctx, rid, drim.SendRoomMessageParams{Type: "text", Body: drim.JSON(`{"text":"点赞"}`), Priority: "low"}))
	check(t, !low.Duplicate, "%+v", low)
	recent := must(im.Chatrooms.RecentMessages(ctx, rid, 0))
	check(t, slices.ContainsFunc(recent, func(m drim.RoomMessage) bool { return m.MessageID == *sent.MessageID }), "最近的消息")
	must(im.Chatrooms.Recall(ctx, rid, *sent.MessageID, "测试"))
	check(t, must(im.Chatrooms.Disable(ctx, rid, "违规")).Status == "disabled", "封禁")
	must(im.Chatrooms.Enable(ctx, rid))
	check(t, must(im.Chatrooms.TransferOwner(ctx, rid, drim.Null[string]())).Owner == nil, "清空所有者")
	check(t, im.Chatrooms.Delete(ctx, rid) == nil && im.Chatrooms.Delete(ctx, rid) == nil, "重复解散")
	check(t, must(im.Chatrooms.Get(ctx, rid)).Status == "dismissed", "已解散")
	for _, n := range []string{owner, admin, user} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestModeration(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	reporter, target := uniq("mo"), uniq("mo")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: reporter}))
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: target, Nickname: "被举报的人"}))
	config := must(im.Moderation.Config(ctx))
	updated := must(im.Moderation.UpdateConfig(ctx, drim.UpdateModerationConfigParams{OnTimeout: drim.String("pass"), Version: drim.Int64(config.Version)}))
	check(t, updated.OnTimeout == "pass", "%+v", updated)
	list := must(im.Moderation.CreateWordList(ctx, drim.CreateWordListParams{Name: uniq("词库"), Action: "reject", Category: "ad"}))
	check(t, list.Kind == "block", "%+v", list)
	check(t, must(im.Moderation.WordList(ctx, list.ListID)).Name == list.Name, "词库")
	renamed := must(im.Moderation.UpdateWordList(ctx, list.ListID, drim.UpdateWordListParams{Scenes: drim.Set([]string{"message"})}))
	check(t, slices.Equal(renamed.Scenes, []string{"message"}), "%+v", renamed)
	check(t, slices.ContainsFunc(must(im.Moderation.WordLists(ctx)), func(w drim.WordList) bool { return w.ListID == list.ListID }), "词库列表")
	added := must(im.Moderation.AddWords(ctx, list.ListID, []string{"加微信", "加微信", "x"}))
	check(t, added.Added == 1, "%+v", added)
	var words []string
	for w, err := range im.Moderation.AllWords(ctx, list.ListID, drim.ListWordsParams{}) {
		words = append(words, must(w, err).Word)
	}
	check(t, slices.Equal(words, []string{"加微信"}), "%v", words)
	checked := must(im.Moderation.CheckText(ctx, drim.CheckTextParams{Scene: "message", Text: "请加微信聊"}))
	check(t, checked.Fields["text"].Action == "reject" && len(checked.Fields["text"].Hits) > 0, "%+v", checked)
	check(t, must(im.Moderation.RemoveWords(ctx, list.ListID, []string{"加微信"})) == 1, "删除词条")
	check(t, im.Moderation.DeleteWordList(ctx, list.ListID) == nil, "删除词库")
	report := must(im.Moderation.Report(ctx, drim.ReportParams{Reporter: reporter, TargetType: "user", Username: target, Reason: "ad", Description: "发广告"}))
	check(t, !report.Duplicate, "%+v", report)
	again := must(im.Moderation.Report(ctx, drim.ReportParams{Reporter: reporter, TargetType: "user", Username: target, Reason: "ad"}))
	check(t, again.Duplicate && again.ReportID == report.ReportID, "%+v", again)
	var item drim.ModerationItem
	for it, err := range im.Moderation.AllItems(ctx, drim.ListItemsParams{TargetType: "user"}) {
		it = must(it, err)
		if it.TargetUsername != nil && *it.TargetUsername == target {
			item = it
		}
	}
	check(t, item.ItemID != "", "举报产生的审核记录")
	content := must(im.Moderation.ItemContent(ctx, item.ItemID))
	check(t, content.ItemID == item.ItemID && len(content.Reports) > 0, "%+v", content)
	decided := must(im.Moderation.Decide(ctx, item.ItemID, drim.DecideParams{Decision: "violation", Note: "确认", Version: drim.Int64(item.Version)}))
	check(t, decided.Status == "violation", "%+v", decided)
	detail := must(im.Moderation.Item(ctx, item.ItemID))
	check(t, detail.Decision != nil && detail.Decision.Result == "violation", "%+v", detail)
	batch := must(im.Moderation.BatchDecide(ctx, drim.BatchDecideParams{ItemIDs: []string{item.ItemID}, Decision: "violation"}))
	check(t, batch.Results[0].Error != nil && batch.Results[0].Error.Code == "version_conflict", "%+v", batch.Results)
	violations := must(im.Moderation.Violations(ctx, target, drim.ListOptions{}))
	check(t, violations.ViolationCount >= 1, "%+v", violations)
	check(t, must(im.Moderation.ClearViolations(ctx, target, "申诉成功")) >= 1, "清除违规")
	for _, err := range im.Moderation.AllPlatformActions(ctx, drim.ListOptions{}) {
		check(t, err == nil, "%v", err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	stats := must(im.Moderation.Stats(ctx, drim.ModerationStatsParams{From: yesterday, To: today}))
	check(t, stats.Raw != nil, "统计")
	_, err := im.Moderation.ProcessPending(ctx, drim.ProcessOptions{}, func(ctx context.Context, item drim.ModerationItem, content drim.ItemContent) (*drim.Decision, error) {
		return &drim.Decision{Decision: "no_violation"}, nil
	})
	check(t, err == nil, "%v", err)
	for _, n := range []string{reporter, target} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestCallsAndPresence(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	a := uniq("c")
	must(im.Users.Create(ctx, drim.CreateUserParams{Username: a}))
	for c, err := range im.Calls.All(ctx, drim.ListCallsParams{Username: a}) {
		t.Fatalf("没有通话：%v %v", c, err)
	}
	active := must(im.Calls.ActiveOf(ctx, a))
	check(t, active.Current == nil && len(active.Incoming) == 0, "%+v", active)
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	must(im.Calls.Stats(ctx, yesterday, today))
	_, err := im.Calls.Get(ctx, "123")
	check(t, drim.IsCode(err, drim.CodeNotFound), "%v", err)
	_, err = im.Calls.End(ctx, "123", "测试")
	check(t, drim.IsCode(err, drim.CodeNotFound), "%v", err)
	presence := must(im.Presence.Query(ctx, []string{a, a, "nobody-" + a}))
	check(t, len(presence) == 1 && presence[0].Username == a && !presence[0].Online, "%+v", presence)
	single := must(im.Presence.Get(ctx, a))
	check(t, !single.Online && len(single.Platforms) == 0, "%+v", single)
	_ = im.Users.Delete(ctx, a)
}
