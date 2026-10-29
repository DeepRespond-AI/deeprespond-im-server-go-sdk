//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func TestUsers(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	name := uniq("u")
	user := must(im.Users.Create(ctx, drim.CreateUserParams{Username: name, Nickname: "张三", Attributes: map[string]string{"vip": "1", "level": "3"}}))
	check(t, user.Username == name && user.Nickname == "张三" && user.Version >= 1, "%+v", user)
	_, err := im.Users.Create(ctx, drim.CreateUserParams{Username: name})
	check(t, drim.IsCode(err, drim.CodeAlreadyExists), "%v", err)
	got := must(im.Users.Get(ctx, strings.ToUpper(name))) // 用户名不区分大小写
	check(t, got.Username == name, got.Username)
	updated := must(im.Users.Update(ctx, name, drim.UpdateUserParams{
		Attributes: drim.Set(map[string]drim.Field[string]{"level": drim.Null[string](), "city": drim.Set("北京")}),
		Version:    drim.Int64(got.Version),
	}))
	check(t, len(updated.Attributes) == 2 && updated.Attributes["city"] == "北京" && updated.Attributes["vip"] == "1", "%v", updated.Attributes)
	_, err = im.Users.Update(ctx, name, drim.UpdateUserParams{Nickname: drim.String("x"), Version: drim.Int64(got.Version)})
	check(t, drim.IsCode(err, drim.CodeVersionConflict), "%v", err)
	cleared := must(im.Users.Update(ctx, name, drim.UpdateUserParams{Attributes: drim.Null[map[string]drim.Field[string]](), Nickname: drim.String("")}))
	check(t, len(cleared.Attributes) == 0 && cleared.Nickname == "", "%+v", cleared)
	_, err = im.Users.Update(ctx, name, drim.UpdateUserParams{})
	check(t, drim.IsCode(err, drim.CodeInvalidArgument), "没有要改的字段：%v", err)
	found := must(im.Users.Lookup(ctx, []string{name, "nobody-" + name, name}))
	check(t, len(found) == 1 && found[0].Username == name, "%v", found)
	page := must(im.Users.List(ctx, drim.ListUsersParams{UsernamePrefix: name[:6], ListOptions: drim.ListOptions{Limit: 5}}))
	check(t, slices.ContainsFunc(page.Items, func(u drim.User) bool { return u.Username == name }), "列表中没有")
	var names []string
	for u, err := range im.Users.All(ctx, drim.ListUsersParams{UsernamePrefix: name}) {
		names = append(names, must(u, err).Username)
	}
	check(t, len(names) == 1 && names[0] == name, "%v", names)

	check(t, im.Users.SetPassword(ctx, name, "Secret-123") == nil, "设置密码")
	check(t, must(im.Users.Get(ctx, name)).HasPassword, "has_password")
	disabled := must(im.Users.Disable(ctx, name, drim.DisableUserParams{Reason: "spam", DurationSeconds: drim.Int64(3600)}))
	check(t, disabled.Status == "disabled" && disabled.StatusReason != nil && *disabled.StatusReason == "spam", "%+v", disabled)
	_, err = im.Users.IssueLoginTicket(ctx, name, drim.IssueLoginTicketParams{})
	check(t, drim.IsCode(err, drim.CodeUserDisabled), "%v", err)
	check(t, must(im.Users.Enable(ctx, name)).Status == "active", "解封")
	ticket := must(im.Users.IssueLoginTicket(ctx, name, drim.IssueLoginTicketParams{}))
	check(t, strings.HasPrefix(ticket.Ticket, "ult_") && ticket.ExpiresIn == 300 && !ticket.UserCreated, "%v", ticket)
	auto := uniq("u")
	check(t, must(im.Users.IssueLoginTicket(ctx, auto, drim.IssueLoginTicketParams{AutoCreate: true})).UserCreated, "自动创建")
	check(t, len(must(im.Users.Sessions(ctx, name))) == 0, "没有登录设备")
	check(t, im.Users.KickAll(ctx, name) == nil, "踢掉全部设备")
	check(t, drim.IsCode(im.Users.Kick(ctx, name, "123"), drim.CodeNotFound), "踢掉不存在的会话")

	mute := must(im.Users.Mute(ctx, name, "chat", drim.MuteParams{DurationSeconds: drim.Int64(600), Reason: "刷屏"}))
	check(t, mute.Scope == "chat" && mute.ExpiresAt != nil && mute.Reason == "刷屏", "%+v", mute)
	mutes := must(im.Users.Mutes(ctx, name))
	check(t, len(mutes) == 1 && mutes[0].Scope == "chat", "%v", mutes)
	count := 0
	for m, err := range im.Users.AllMuted(ctx, drim.ListMutedParams{Scope: "chat"}) {
		if must(m, err).Username == name {
			count++
		}
	}
	check(t, count == 1, "全局禁言的用户：%d", count)
	check(t, im.Users.Unmute(ctx, name, "chat") == nil, "解除禁言")
	check(t, im.Users.Unmute(ctx, name, "chat") == nil, "没有禁言时也是 204")
	check(t, len(must(im.Users.Mutes(ctx, name))) == 0, "已解除")

	check(t, im.Users.Delete(ctx, name) == nil && im.Users.Delete(ctx, auto) == nil, "删除")
	_, err = im.Users.Get(ctx, name)
	check(t, drim.IsCode(err, drim.CodeNotFound), "%v", err)
}

func TestBatchCreateAll(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	users := make([]drim.CreateUserParams, 130)
	for i := range users {
		users[i] = drim.CreateUserParams{Username: uniq("b")}
	}
	must(im.Users.Create(ctx, users[5]))
	result := must(im.Users.BatchCreateAll(ctx, users))
	check(t, len(result.Created) == 129, "%d", len(result.Created))
	check(t, len(result.Failed) == 1 && result.Failed[0].Key == users[5].Username && result.Failed[0].Code == "already_exists", "%+v", result.Failed)
	for _, u := range users {
		_ = im.Users.Delete(ctx, u.Username)
	}
}

func TestFriendsAndBlacklist(t *testing.T) {
	im := newClient(t)
	ctx := context.Background()
	a, b, c, d := uniq("f"), uniq("f"), uniq("f"), uniq("f")
	for _, n := range []string{a, b, c, d} {
		must(im.Users.Create(ctx, drim.CreateUserParams{Username: n}))
	}
	change := must(im.Friends.Add(ctx, a, b, drim.AddFriendParams{Remark: "老同学", Attributes: drim.Set(map[string]drim.Field[string]{"tag": drim.Set("school")}), AddSource: "import"}))
	check(t, change.Changed && change.Friend != nil && change.Friend.Username == b, "%+v", change)
	check(t, !must(im.Friends.Add(ctx, a, b, drim.AddFriendParams{})).Changed, "重复添加")
	friend := must(im.Friends.Get(ctx, a, b))
	check(t, friend.Remark == "老同学" && friend.Attributes["tag"] == "school", "%+v", friend)
	updated := must(im.Friends.Update(ctx, a, b, drim.UpdateFriendParams{
		Remark: drim.String(""), Attributes: drim.Set(map[string]drim.Field[string]{"tag": drim.Null[string]()}),
	}))
	check(t, updated.Friend != nil && updated.Friend.Remark == "", "%+v", updated)
	page := must(im.Friends.List(ctx, a, drim.ListFriendsParams{}))
	check(t, page.Count == 1 && len(page.Items) == 1 && page.Items[0].Username == b && page.Version > 0, "%+v", page)
	cached := must(im.Friends.List(ctx, a, drim.ListFriendsParams{KnownVersion: drim.Int64(page.Version)}))
	check(t, cached.NotModified && len(cached.Items) == 0, "%+v", cached)
	checks := must(im.Friends.Check(ctx, a, []string{b, c}))
	check(t, len(checks) == 2 && checks[0].IsFriend && !checks[1].IsFriend, "%+v", checks)
	batch := must(im.Friends.BatchAdd(ctx, a, []drim.AddFriendItem{{Username: c}, {Username: "nobody-" + a}}))
	check(t, len(batch.Added) == 1 && batch.Added[0].Username == c && len(batch.Failed) == 1 && batch.Failed[0].Key == "nobody-"+a, "%+v", batch)
	removed := must(im.Friends.BatchDelete(ctx, a, []string{c}))
	check(t, len(removed.Deleted) == 1 && removed.Deleted[0] == c, "%+v", removed)
	for r, err := range im.Friends.AllRequests(ctx, a, drim.ListFriendRequestsParams{Direction: "received"}) {
		t.Fatalf("没有申请：%v %v", r, err)
	}
	settings := must(im.Friends.SetSettings(ctx, a, drim.Set("deny_any")))
	check(t, settings.FriendAddMode != nil && *settings.FriendAddMode == "deny_any", "%+v", settings)
	reset := must(im.Friends.SetSettings(ctx, a, drim.Null[string]()))
	check(t, reset.FriendAddMode == nil && reset.EffectiveFriendAddMode != "", "%+v", reset)
	check(t, must(im.Friends.Settings(ctx, a)).FriendAddMode == nil, "跟随应用")
	must(im.Friends.BatchAdd(ctx, a, []drim.AddFriendItem{{Username: c}, {Username: d}}))
	check(t, must(im.Friends.DeleteAll(ctx, a)) == 3, "删除全部好友")
	check(t, !must(im.Friends.Delete(ctx, a, b)).Changed, "不是好友时 changed 为假")

	blocked := must(im.Blacklist.Add(ctx, a, b))
	check(t, blocked.Changed && blocked.Entry != nil, "%+v", blocked)
	listed := must(im.Blacklist.List(ctx, a, drim.ListBlacklistParams{}))
	check(t, len(listed.Items) == 1 && listed.Items[0].Username == b && listed.MaxCount == 1000, "%+v", listed)
	many := must(im.Blacklist.BatchAdd(ctx, a, []string{c, d}))
	sort.Strings(many.Added)
	want := []string{c, d}
	sort.Strings(want)
	check(t, slices.Equal(many.Added, want), "%v", many.Added)
	gone := must(im.Blacklist.BatchRemove(ctx, a, []string{c, d}))
	check(t, len(gone.Removed) == 2, "%v", gone.Removed)
	check(t, must(im.Blacklist.Remove(ctx, a, b)).Changed, "移出黑名单")
	for e, err := range im.Blacklist.All(ctx, a, drim.ListBlacklistParams{}) {
		t.Fatalf("黑名单应为空：%v %v", e, err)
	}
	for _, n := range []string{a, b, c, d} {
		_ = im.Users.Delete(ctx, n)
	}
}

func TestNotFoundHasRequestID(t *testing.T) {
	im := newClient(t)
	_, err := im.Users.Get(context.Background(), "nobody-"+uniq("x"))
	var e *drim.Error
	check(t, errors.As(err, &e) && e.Code == drim.CodeNotFound && e.RequestID != "", "%v", err)
}
