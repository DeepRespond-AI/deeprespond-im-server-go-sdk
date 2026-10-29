package main

// 签名回调：刷新相关列表、同步消息或核对有效设备。

import (
	"context"
	"log"
	"sort"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

func (st *AppState) buildCallbackHandler() (*callback.Handler, error) {
	return callback.NewHandler(callback.Config{
		Secrets: callback.StaticSecrets(st.callbackSecrets),
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Events:  st.eventHandlers(),
	})
}

func (st *AppState) eventHandlers() callback.EventHandlers {
	return callback.EventHandlers{
		MessageSent: func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
			sender, recipient, group := "", "", ""
			if d.SenderUsername != nil {
				sender = *d.SenderUsername
			}
			if d.RecipientUsername != nil {
				recipient = *d.RecipientUsername
			}
			if d.GroupID != nil {
				group = *d.GroupID
			}
			st.mu.Lock()
			var names []string
			for name, c := range st.contexts {
				if len(c.Sessions) > 0 && (group != "" || name == sender || name == recipient) {
					names = append(names, name)
				}
			}
			st.mu.Unlock()
			for _, name := range names {
				if err := st.syncMessage(name, d.ConversationID, d.Seq, ""); err != nil {
					return err
				}
			}
			return nil
		},
		MessageRecalled: func(ctx context.Context, ev *callback.Event, d *callback.MessageRecalled) error {
			return st.syncChangedMessage(d.ConversationID, d.Seq, "recalled")
		},
		MessageEdited: func(ctx context.Context, ev *callback.Event, d *callback.MessageEdited) error {
			return st.syncChangedMessage(d.ConversationID, d.Seq, "edited")
		},

		FriendAdded: func(ctx context.Context, ev *callback.Event, d *callback.FriendAdded) error {
			for _, username := range uniqNonEmpty(d.Username, d.FriendUsername) {
				if uctx := st.userContext(username); uctx != nil {
					_ = st.refreshFriends(uctx)
					st.mu.Lock()
					st.pushFriendsLocked(uctx)
					st.mu.Unlock()
					st.resyncConvs(uctx)
				}
			}
			return nil
		},

		FriendRemoved: func(ctx context.Context, ev *callback.Event, d *callback.FriendRemoved) error {
			names := uniqNonEmpty(d.Username)
			if d.FriendUsername != nil {
				names = uniqNonEmpty(d.Username, *d.FriendUsername)
			}
			for _, username := range names {
				if uctx := st.userContext(username); uctx != nil {
					_ = st.refreshFriends(uctx)
					st.mu.Lock()
					st.pushFriendsLocked(uctx)
					st.mu.Unlock()
					st.mu.Lock()
					st.pushConvsLocked(uctx)
					st.mu.Unlock()
				}
			}
			return nil
		},

		FriendUpdated: func(ctx context.Context, ev *callback.Event, d *callback.FriendUpdated) error {
			for _, username := range uniqNonEmpty(d.Username, d.FriendUsername) {
				if uctx := st.userContext(username); uctx != nil {
					_ = st.refreshFriends(uctx)
					st.mu.Lock()
					st.pushFriendsLocked(uctx)
					st.pushConvsLocked(uctx)
					st.mu.Unlock()
				}
			}
			return nil
		},

		FriendRequestReceived: func(ctx context.Context, ev *callback.Event, d *callback.FriendRequestReceived) error {
			if uctx := st.userContext(d.ToUsername); uctx != nil {
				st.pushRequests(uctx)
			}
			return nil
		},

		BlacklistChanged: func(ctx context.Context, ev *callback.Event, d *callback.BlacklistChanged) error {
			if uctx := st.userContext(d.Username); uctx != nil {
				st.pushBlacklist(uctx)
			}
			return nil
		},

		GroupCreated: func(ctx context.Context, ev *callback.Event, d *callback.GroupCreated) error {
			var extras []string
			if d.OwnerUsername != nil && *d.OwnerUsername != "" {
				extras = append(extras, *d.OwnerUsername)
			}
			st.onGroupEvent(d.GroupID, extras, true)
			return nil
		},

		GroupInfoChanged: func(ctx context.Context, ev *callback.Event, d *callback.GroupInfoChanged) error {
			st.onGroupEvent(d.GroupID, nil, false)
			return nil
		},

		GroupDismissed: func(ctx context.Context, ev *callback.Event, d *callback.GroupDismissed) error {
			st.onGroupEvent(d.GroupID, nil, true)
			return nil
		},

		GroupMembersAdded: func(ctx context.Context, ev *callback.Event, d *callback.GroupMembersAdded) error {
			var extras []string
			for _, m := range d.Members {
				if m.Username != nil {
					extras = append(extras, *m.Username)
				}
			}
			st.onGroupEvent(d.GroupID, extras, true)
			return nil
		},

		GroupMembersRemoved: func(ctx context.Context, ev *callback.Event, d *callback.GroupMembersRemoved) error {
			var extras []string
			for _, m := range d.Members {
				if m.Username != nil {
					extras = append(extras, *m.Username)
				}
			}
			st.onGroupEvent(d.GroupID, extras, true)
			return nil
		},

		GroupMembersUpdated: func(ctx context.Context, ev *callback.Event, d *callback.GroupMembersUpdated) error {
			st.onGroupEvent(d.GroupID, nil, false)
			return nil
		},

		UserProfileChanged: func(ctx context.Context, ev *callback.Event, d *callback.UserProfileChanged) error {
			if uctx := st.userContext(d.Username); uctx != nil {
				if me, err := st.client.Users.Get(ctx, d.Username); err == nil {
					st.mu.Lock()
					uctx.Me = rawMap(me.Raw)
					st.pushLocked(d.Username, "profile", uctx.Me)
					st.mu.Unlock()
				}
			}
			// 把该用户加了好友的其他在线用户：刷新好友列表并推送（昵称、头像可能变了）
			st.mu.Lock()
			var others []*UserContext
			for _, other := range st.contexts {
				if other.Username != d.Username {
					if _, ok := other.Friends[d.Username]; ok {
						others = append(others, other)
					}
				}
			}
			st.mu.Unlock()
			for _, other := range others {
				if err := st.refreshFriends(other); err != nil {
					continue
				}
				st.mu.Lock()
				st.pushFriendsLocked(other)
				st.pushConvsLocked(other)
				st.mu.Unlock()
			}
			return nil
		},

		UserSessionsRevoked: func(ctx context.Context, ev *callback.Event, d *callback.UserSessionsRevoked) error {
			if len(d.SessionIDs) == 0 {
				return st.reconcileSessions(d.Username, "登录状态已失效")
			}
			ids := map[string]bool{}
			for _, id := range d.SessionIDs {
				ids[id] = true
			}
			st.endBrowserSessions(d.Username, "登录状态已失效", ids)
			return nil
		},
		UserReloginRequired: func(ctx context.Context, ev *callback.Event, d *callback.UserReloginRequired) error {
			st.mu.Lock()
			var names []string
			for name, c := range st.contexts {
				if len(c.Sessions) > 0 {
					names = append(names, name)
				}
			}
			st.mu.Unlock()
			for _, name := range names {
				if err := st.reconcileSessions(name, "需要重新登录"); err != nil {
					return err
				}
			}
			return nil
		},

		PresenceChanged: func(ctx context.Context, ev *callback.Event, d *callback.PresenceChanged) error {
			view := presenceView(map[string]any{
				"username":  d.Username,
				"online":    d.Online,
				"platforms": d.Platforms,
				"version":   d.Version,
			})
			st.mu.Lock()
			defer st.mu.Unlock()
			for _, uctx := range st.contexts {
				if _, ok := uctx.Friends[d.Username]; ok || uctx.Username == d.Username {
					st.pushLocked(uctx.Username, "presence", []map[string]any{view})
				}
			}
			return nil
		},
	}
}

// userContext 读取已登录过的用户，回调不为无关用户创建缓存。
func (st *AppState) userContext(username string) *UserContext {
	if username == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.contexts[username]
}

// onGroupEvent 群事件：刷新相关用户的群列表（缓存里有这个群的用户 + extras 提到的用户），
// 推送列表；resync 时重跑会话同步（建群、加人、退群、解散都会产生或改变会话）。
func (st *AppState) onGroupEvent(groupID string, extras []string, resync bool) {
	if groupID == "" {
		return
	}
	st.mu.Lock()
	targets := map[string]bool{}
	for _, ctx := range st.contexts {
		if _, ok := ctx.Groups[groupID]; ok {
			targets[ctx.Username] = true
		}
	}
	for _, u := range extras {
		if u != "" {
			targets[u] = true
		}
	}
	st.mu.Unlock()

	for _, username := range sortedKeys(targets) {
		uctx := st.userContext(username)
		if uctx == nil {
			continue
		}
		if err := st.refreshGroups(uctx); err != nil {
			log.Printf("[%s] 刷新群列表失败: %v", username, errText(err))
			continue
		}
		st.mu.Lock()
		st.pushGroupsLocked(uctx)
		st.mu.Unlock()
		if resync {
			st.resyncConvs(uctx)
		} else {
			st.mu.Lock()
			st.pushConvsLocked(uctx)
			st.mu.Unlock()
		}
	}
}

// uniqNonEmpty 用于好友和群回调的用户名去重。
func uniqNonEmpty(names ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func mergeMaps(base, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		out[k] = v
	}
	return out
}
