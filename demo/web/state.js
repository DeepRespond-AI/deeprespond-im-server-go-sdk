// 共享页面状态与消息合并；不发请求、不操作 DOM。

export const S = {
  me: null,
  selfSession: "",
  convs: [],
  friends: [],
  requests: { received: [], sent: [], unread: 0 },
  groups: [],
  invitations: [],
  blacklist: [],
  presence: {},
  tab: "chat",
  openConv: null,
  convMeta: {},        // convId → {friend, group}
  messages: {},        // convId → Message[]
  historyPages: {},    // convId → 服务端历史分页游标（空页也可能仍有下一页）
  typing: {},          // convId → {username, ts}
  conn: "disconnected",
  booted: false,       // 是否已收到过 bootstrap（区分「未登录」与「已登录后瞬断」）
  replyTo: null,
  kicked: false,
  detail: null,        // 通讯录/群组右侧详情 {kind, data}
  members: {},         // groupId → GroupMember[]
};

export function currentConv() {
  if (S.openConv && String(S.openConv).startsWith("peer:")) {
    const peer = S.openConv.slice(5);
    const f = S.friends.find((x) => x.username === peer);
    return { id: S.openConv, type: "single", peer, name: (f && (f.remark || f.nickname)) || peer,
      unread: 0, marked_unread: false, pinned: false, max_seq: 0, last_seq: 0, peer_read_seq: 0 };
  }
  return S.convs.find((c) => c.id === S.openConv) || null;
}

export function upsertMessage(arr, m) {
  const i = arr.findIndex((x) => x.seq === m.seq);
  if (i >= 0) arr[i] = m; else {
    arr.push(m);
    arr.sort((a, b) => a.seq - b.seq);
  }
}
