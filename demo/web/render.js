// 会话列表、通讯录、群资料和账号面板。
import { bridge } from "./bridge.js";
import { renderChat } from "./chat.js";
import { S } from "./state.js";
import { $, $$, avHTML, displayNameOf, esc, fmtListTime } from "./ui.js";

export function renderAll() {
  renderListPane();
  renderMain();
  updateBadges();
}

export function updateBadges() {
  const unread = S.convs.reduce((n, c) => n + (c.unread > 0 ? c.unread : c.marked_unread ? 1 : 0), 0);
  setBadge("#badge-chat", unread);
  setBadge("#badge-contacts", S.requests.unread || 0);
  setBadge("#badge-groups", S.invitations.length);
}

function setBadge(sel, n) {
  const el = $(sel);
  el.textContent = n > 99 ? "99+" : n > 0 ? n : "";
  el.classList.toggle("hidden", !(n > 0));
}

export function renderListPane() {
  const el = $("#pane-list");
  if (S.tab === "chat") el.innerHTML = chatListHTML();
  else if (S.tab === "contacts") el.innerHTML = contactsHTML();
  else if (S.tab === "groups") el.innerHTML = groupsHTML();
  else el.innerHTML = meHTML();
  $$(".nav-btn").forEach((b) => b.classList.toggle("active", b.dataset.tab === S.tab));
}

export function renderConvList() { renderListPane(); }

export function renderMain() {
  const el = $("#pane-main");
  if (S.tab === "me") { el.innerHTML = meDetailHTML(); loadSessions(); return; }
  if (S.tab === "contacts" && S.detail && S.detail.kind === "friend") { el.innerHTML = friendDetailHTML(S.detail.data); return; }
  if (S.tab === "groups" && S.detail && S.detail.kind === "group") { renderGroupDetail(); return; }
  if (S.openConv) { renderChat(); return; }
  el.innerHTML = `<div class="main-empty">${esc(S.tab === "contacts" ? "选择一位好友查看资料" : S.tab === "groups" ? "选择一个群查看资料" : "选择一个会话开始聊天")}</div>`;
}

function chatListHTML() {
  const rows = S.convs.map((c) => {
    const t = S.typing[c.id];
    const typing = t && Date.now() - t.ts < 5000 && Date.now() - (t.firstSeen || t.ts) < 15000 && t.username !== (S.me && S.me.username);
    let preview = c.last_preview || " ";
    if (typing) preview = `${esc(displayNameOf(t.username))} 正在输入…`;
    else if (c.last_sender === "self" && c.last_seq > 0) preview = "我: " + preview;
    const isGroup = c.type === "group";
    return `
    <div class="row conv-row ${c.id === S.openConv ? "active" : ""} ${c.pinned ? "pinned" : ""}" data-act="conv-open" data-id="${esc(c.id)}"
         style="${c.pinned ? "background:#f2f2f2" : ""}">
      ${isGroup ? avHTML(c.name || c.group_id, "", false) : avHTML(displayNameOf(c.peer), "", true)}
      <div class="row-main">
        <div class="row-top">
          <span class="row-title">${esc(c.name || c.peer)}</span>
          <span class="row-time">${esc(fmtListTime(c.last_at))}</span>
        </div>
        <div class="row-sub">
          <span class="preview ${typing ? "typing" : ""}">${esc(preview)}</span>
          ${c.unread > 0 ? `<span class="badge">${c.unread > 99 ? "99+" : c.unread}</span>` :
            c.marked_unread ? `<span class="badge" style="background:#c8c8c8;min-width:10px;padding:0"></span>` : ""}
        </div>
      </div>
      <button class="row-more" data-act="conv-menu" data-id="${esc(c.id)}">⋯</button>
    </div>`;
  });
  return `
    <div class="list-head"><h2>聊天</h2></div>
    <div class="list-scroll">
      ${rows.length ? rows.join("") : `<div class="empty-tip">暂无会话<br><br>去「通讯录」添加好友，或「群组」创建群聊</div>`}
    </div>`;
}

function contactsHTML() {
  const reqs = (S.requests.received || []).filter((r) => r.status === "pending");
  const reqRows = reqs.map((r) => `
    <div class="row disabled">
      ${avHTML(r.nickname || r.username)}
      <div class="row-main">
        <div class="row-top"><span class="row-title">${esc(r.nickname || r.username)}</span><span class="row-time">${esc(fmtListTime(r.requested_at))}</span></div>
        <div class="row-sub"><span class="preview">${esc(r.message || "请求添加你为好友")}</span></div>
      </div>
      <div class="row-btns">
        <button class="btn-plain" data-act="req-accept" data-username="${esc(r.username)}">接受</button>
        <button class="btn-plain" data-act="req-decline" data-username="${esc(r.username)}">拒绝</button>
      </div>
    </div>`).join("");
  const friendRows = S.friends.map((f) => `
    <div class="row friend-row ${S.detail && S.detail.kind === "friend" && S.detail.data.username === f.username ? "active" : ""}"
         data-act="friend-open" data-username="${esc(f.username)}">
      ${avHTML(f.remark || f.nickname || f.username, "", true)}
      <div class="row-main">
        <div class="row-top"><span class="row-title">${esc(f.remark || f.nickname || f.username)}</span></div>
        <div class="row-sub"><span class="preview">微信号：${esc(f.username)}</span></div>
      </div>
    </div>`).join("");
  const blRows = S.blacklist.map((u) => `
    <div class="row disabled">
      ${avHTML(u)}
      <div class="row-main"><div class="row-top"><span class="row-title">${esc(displayNameOf(u))}</span></div></div>
      <button class="btn-plain" data-act="bl-remove" data-username="${esc(u)}">移出黑名单</button>
    </div>`).join("");
  return `
    <div class="list-head"><h2>通讯录</h2></div>
    <div class="search-bar">
      <input id="lookup-input" placeholder="输入用户名查找并添加">
      <button class="btn-plain" data-act="lookup-btn">查找</button>
    </div>
    <div id="lookup-result"></div>
    <div class="list-scroll">
      ${reqRows ? `<div class="list-section">新的朋友（${reqs.length}）</div>${reqRows}` : ""}
      <div class="list-section">好友（${S.friends.length}）</div>
      ${friendRows || `<div class="empty-tip">还没有好友</div>`}
      ${blRows ? `<div class="list-section">黑名单（${S.blacklist.length}）</div>${blRows}` : ""}
    </div>`;
}

function friendDetailHTML(f) {
  const shown = f.remark || f.nickname || f.username;
  return `
  <div class="detail">
    <div class="chat-head" style="margin:-28px -24px 20px">
      <button class="chat-back" data-act="back"><svg viewBox="0 0 24 24"><path d="M15.5 4.5 8 12l7.5 7.5-1.8 1.8L4.4 12l9.3-9.3z"/></svg></button>
      <div class="chat-title">用户资料</div>
    </div>
    <div class="detail-head">
      ${avHTML(shown, "lg")}
      <div class="info">
        <div class="detail-name">${esc(shown)}</div>
        <div class="detail-sub">微信号：${esc(f.username)}</div>
      </div>
    </div>
    ${f.nickname && f.remark ? `<div class="detail-field"><span class="k">昵称</span><span>${esc(f.nickname)}</span></div>` : ""}
    ${f.remark ? `<div class="detail-field"><span class="k">备注</span><span>${esc(f.remark)}</span></div>` : ""}
    <div class="detail-actions">
      <button class="btn-plain" data-act="friend-msg" data-username="${esc(f.username)}">发消息</button>
      <button class="btn-plain" data-act="friend-remark" data-username="${esc(f.username)}" data-remark="${esc(f.remark || "")}">设置备注</button>
      <button class="btn-plain" data-act="friend-block" data-username="${esc(f.username)}">加入黑名单</button>
      <button class="btn-plain btn-danger" data-act="friend-remove" data-username="${esc(f.username)}">删除好友</button>
    </div>
  </div>`;
}

function groupsHTML() {
  const invRows = S.invitations.map((inv) => `
    <div class="row disabled">
      ${avHTML(inv.group_name || inv.group_id || "群")}
      <div class="row-main">
        <div class="row-top"><span class="row-title">${esc(inv.group_name || "群聊")}</span></div>
        <div class="row-sub"><span class="preview">${esc(inv.inviter || "")} 邀请你加入群聊</span></div>
      </div>
      <div class="row-btns">
        <button class="btn-plain" data-act="inv-accept" data-iid="${esc(inv.invitation_id)}">接受</button>
        <button class="btn-plain" data-act="inv-decline" data-iid="${esc(inv.invitation_id)}">拒绝</button>
      </div>
    </div>`).join("");
  const groupRows = S.groups.map((g) => `
    <div class="row group-row ${S.detail && S.detail.kind === "group" && S.detail.data.group_id === g.group_id ? "active" : ""}"
         data-act="group-open" data-gid="${esc(g.group_id)}">
      ${avHTML(g.name || g.group_id)}
      <div class="row-main">
        <div class="row-top"><span class="row-title">${esc(g.name || g.group_id)}</span></div>
        <div class="row-sub"><span class="preview">${g.member_count || 0} 人${g.my_role === "owner" ? " · 我是群主" : g.my_role === "admin" ? " · 我是管理员" : ""}</span></div>
      </div>
    </div>`).join("");
  return `
    <div class="list-head">
      <h2>群组</h2>
      <button class="btn-plain" data-act="group-create">＋ 创建群聊</button>
    </div>
    <div class="list-scroll">
      ${invRows ? `<div class="list-section">群邀请（${S.invitations.length}）</div>${invRows}` : ""}
      <div class="list-section">我的群聊（${S.groups.length}）</div>
      ${groupRows || `<div class="empty-tip">还没有加入任何群聊</div>`}
    </div>`;
}

export async function renderGroupDetail() {
  const el = $("#pane-main");
  if (!S.detail || S.detail.kind !== "group") return;
  const g = S.groups.find((x) => x.group_id === S.detail.data.group_id) || S.detail.data;
  let members = S.members[g.group_id];
  if (!members) {
    el.innerHTML = `<div class="detail"><div class="empty-tip">加载中…</div></div>`;
    try { members = await bridge("group_members", { group_id: g.group_id }); S.members[g.group_id] = members; }
    catch (err) { el.innerHTML = `<div class="detail"><div class="empty-tip">${esc(err.message)}</div></div>`; return; }
    if (!S.detail || S.detail.kind !== "group" || S.detail.data.group_id !== g.group_id) return;
  }
  const isOwner = g.my_role === "owner" || g.my_role === "admin";
  const memberCells = members.map((m) => `
    <div class="member-cell">${avHTML(m.nickname || m.username, "sm")}
      <div class="member-name">${esc(m.nickname || m.username)}${m.role === "owner" ? "（群主）" : m.role === "admin" ? "（管理员）" : ""}</div>
    </div>`).join("");
  el.innerHTML = `
  <div class="detail">
    <div class="chat-head" style="margin:-28px -24px 20px">
      <button class="chat-back" data-act="back"><svg viewBox="0 0 24 24"><path d="M15.5 4.5 8 12l7.5 7.5-1.8 1.8L4.4 12l9.3-9.3z"/></svg></button>
      <div class="chat-title">群聊资料</div>
    </div>
    <div class="detail-head">
      ${avHTML(g.name || g.group_id, "lg")}
      <div class="info">
        <div class="detail-name">${esc(g.name || g.group_id)}</div>
        <div class="detail-sub">${g.member_count || members.length} 人 · ${esc(g.my_role === "owner" ? "我是群主" : g.my_role === "admin" ? "我是管理员" : "成员")}</div>
      </div>
    </div>
    <div class="detail-field"><span class="k">群 ID</span><span>${esc(g.group_id)}</span></div>
    ${g.description ? `<div class="detail-field"><span class="k">群公告</span><span>${esc(g.description)}</span></div>` : ""}
    <div style="margin-top:14px;color:var(--text-2);font-size:13px">群成员（${members.length}）</div>
    <div class="member-grid">${memberCells}</div>
    <div class="detail-actions">
      <button class="btn-plain" data-act="group-msg" data-gid="${esc(g.group_id)}">发消息</button>
      <button class="btn-plain" data-act="group-online" data-gid="${esc(g.group_id)}">查看在线人数</button>
      ${isOwner ? `<button class="btn-plain" data-act="group-rename" data-gid="${esc(g.group_id)}" data-name="${esc(g.name || "")}">修改群名</button>` : ""}
      <button class="btn-plain" data-act="group-invite" data-gid="${esc(g.group_id)}">邀请好友</button>
      <button class="btn-plain btn-danger" data-act="group-leave" data-gid="${esc(g.group_id)}">退出群聊</button>
    </div>
  </div>`;
}

function meHTML() {
  return `
    <div class="list-head"><h2>我的</h2></div>
    <div class="list-scroll"><div class="empty-tip">管理个人资料、账号安全和登录设备</div></div>`;
}

function meDetailHTML() {
  const me = S.me || {};
  const connText = S.conn === "connected" ? "已连接" : S.conn === "connecting" ? "连接中…" : "已断开";
  return `
  <div class="detail" style="padding:0;overflow-y:auto;height:100%">
    <div class="chat-head">
      <button class="chat-back" data-act="back"><svg viewBox="0 0 24 24"><path d="M15.5 4.5 8 12l7.5 7.5-1.8 1.8L4.4 12l9.3-9.3z"/></svg></button>
      <div class="chat-title">我的</div>
    </div>
    <div class="me-card">
      ${avHTML(me.nickname || me.username || "?", "lg")}
      <div class="info">
        <div class="detail-name">${esc(me.nickname || me.username || "")}</div>
        <div class="detail-sub">微信号：${esc(me.username || "")} · <span style="color:${S.conn === "connected" ? "var(--green)" : "var(--red)"}">${connText}</span></div>
      </div>
      <button class="btn-plain" data-act="me-edit-nick">修改昵称</button>
    </div>
    <div class="me-section">
      <h3>账号安全</h3>
      <div class="passwd-form" id="passwd-form">
        <input type="password" id="pw-old" placeholder="当前密码">
        <input type="password" id="pw-new" placeholder="新密码（至少 6 位）">
        <div id="pw-msg" class="hidden"></div>
        <button class="btn-plain" data-act="pw-submit">修改密码</button>
      </div>
    </div>
    <div class="me-section">
      <h3>设备与登录态 <button class="btn-plain" style="font-size:12px" data-act="sessions-refresh">刷新</button></h3>
      <div id="sessions-box"><div class="empty-tip">加载中…</div></div>
    </div>
    <div class="logout-bar">
      <button class="btn-primary" style="background:#fff;color:var(--red);border:1px solid #f0d0d0" data-act="me-logout">退出登录</button>
    </div>
  </div>`;
}

export async function loadSessions() {
  const box = $("#sessions-box");
  if (!box) return;
  try {
    const d = await bridge("sessions");
    box.innerHTML = (d.items || []).map((s) => {
      const cur = s.session_id === d.current;
      return `
      <div class="me-row">
        <div class="k">${esc(s.platform || "web")} · ${esc(s.device_name || s.device_id || "")}</div>
        <div class="v">${cur ? "本机" : esc((s.session_id || "").slice(0, 18))}</div>
        <button class="op ${cur ? "danger" : ""}" data-act="session-revoke" data-sid="${esc(s.session_id)}">${cur ? "下线本机" : "下线"}</button>
      </div>`;
    }).join("") || `<div class="empty-tip">暂无</div>`;
  } catch (err) {
    box.innerHTML = `<div class="empty-tip">${esc(err.message)}</div>`;
  }
}
