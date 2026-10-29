// 聊天窗口、消息渲染、历史翻页和发送。
import { bridge } from "./bridge.js";
import { S, currentConv, upsertMessage } from "./state.js";
import { $, avHTML, displayNameOf, esc, fmtChatTime, fmtSize, mediaSrc, toast } from "./ui.js";

export function renderChat() {
  const c = currentConv();
  if (!c) return;
  const el = $("#pane-main");
  const isGroup = c.type === "group";
  const meta = S.convMeta[c.id] || {};
  const title = isGroup ? (c.name || (meta.group && meta.group.name) || c.group_id) : (c.name || c.peer);
  el.innerHTML = `
  <div class="chat">
    <div class="chat-head">
      <button class="chat-back" data-act="back"><svg viewBox="0 0 24 24"><path d="M15.5 4.5 8 12l7.5 7.5-1.8 1.8L4.4 12l9.3-9.3z"/></svg></button>
      <div class="chat-title"><span id="chat-title-text">${esc(title)}</span><span class="chat-sub" id="chat-sub"></span></div>
      ${isGroup ? `<button class="btn-plain" data-act="group-info" data-gid="${esc(c.group_id)}">群资料</button>` : ""}
    </div>
    <div class="chat-body" id="chat-body"></div>
    <div class="chat-foot">
      <div id="reply-bar-slot"></div>
      <div class="chat-tools">
        <button class="tool-btn" data-act="pick-image" title="发送图片">
          <svg viewBox="0 0 24 24"><path d="M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2zm0 12 4.5-5 3.5 4 2.5-3L19 15H5zm3.5-7a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z"/></svg>
        </button>
        <button class="tool-btn" data-act="pick-file" title="发送文件">
          <svg viewBox="0 0 24 24"><path d="M13 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9l-6-6zm-1 7V4.5L17.5 10H12z"/></svg>
        </button>
      </div>
      <div class="chat-input-area">
        <textarea id="chat-input" class="chat-input" rows="1" placeholder="输入消息，Enter 发送，Shift+Enter 换行"></textarea>
        <button class="send-btn" id="send-btn" data-act="send">发送</button>
      </div>
    </div>
  </div>`;
  renderMessages();
  bindChatInput();
}

export function renderChatSub() {
  const el = $("#chat-sub");
  if (!el) return;
  const c = currentConv();
  if (!c) return;
  const t = S.typing[c.id];
  if (t && Date.now() - t.ts < 5000 && Date.now() - (t.firstSeen || t.ts) < 15000 && t.username !== (S.me && S.me.username)) {
    el.textContent = `${displayNameOf(t.username)} 正在输入…`;
    return;
  }
  el.textContent = c.type === "group" ? (S.convMeta[c.id] && S.convMeta[c.id].group ? `${S.convMeta[c.id].group.member_count || ""} 人` : "") : "";
}

function msgDayKey(m) { return (m.created_at || "").slice(0, 10); }

export function renderMessages() {
  const body = $("#chat-body");
  if (!body) return;
  const c = currentConv();
  if (!c) return;
  const msgs = S.messages[c.id] || [];
  const self = S.me && S.me.username;
  const lastSelfSeq = (() => {
    for (let i = msgs.length - 1; i >= 0; i--) {
      const m = msgs[i];
      if (m.sender === self && m.sender_type !== "system" && !m.recalled) return m.seq;
    }
    return 0;
  })();
  let html = "";
  if (S.historyPages[c.id]?.has_more) {
    html += `<div style="text-align:center"><button class="btn-plain" data-act="load-history" ${S.historyPages[c.id].loading ? "disabled" : ""}>加载更早的消息</button></div>`;
  }
  let prevDay = "";
  for (const m of msgs) {
    const day = msgDayKey(m);
    if (day && day !== prevDay) { html += `<div class="day-chip"><span>${esc(fmtChatTime(m.created_at))}</span></div>`; prevDay = day; }
    html += msgHTML(m, c, m.seq === lastSelfSeq);
  }
  body.innerHTML = html || `<div class="empty-tip" style="margin-top:40px">还没有消息，打个招呼吧</div>`;
  body.scrollTop = body.scrollHeight;
}

function msgHTML(m, c, isLastSelf) {
  const self = S.me && S.me.username;
  if (m.sender_type === "system" || m.type === "tip") {
    return `<div class="msg-sys"><span>${esc(m.Text || m.text || tipText(m))}</span></div>`;
  }
  if (m.recalled) {
    const who = m.sender === self ? "你" : esc(displayNameOf(m.sender));
    return `<div class="msg-sys"><span>${who}撤回了一条消息</span></div>`;
  }
  const isSelf = m.sender === self;
  const isGroup = c.type === "group";
  const senderName = isSelf ? "" : esc(displayNameOf(m.sender));
  let contentHTML = "";
  if (m.type === "text") {
    contentHTML = esc(bodyText(m)).replace(/\n/g, "<br>");
  } else if (m.type === "image") {
    const b = m.body || {};
    contentHTML = `<img class="msg-image" loading="lazy" src="${esc(mediaSrc(b.url, b.content_type))}" alt="图片" onclick="window.open(this.src)">`;
  } else if (m.type === "file") {
    const b = m.body || {};
    contentHTML = `
      <div class="file-card" data-act="open-file" data-url="${esc(mediaSrc(b.url, b.content_type))}">
        <div class="file-ico"><svg viewBox="0 0 24 24"><path d="M13 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9l-6-6zm-1 7V4.5L17.5 10H12z"/></svg></div>
        <div><div class="file-name">${esc(b.name || "文件")}</div><div class="file-size">${fmtSize(b.size || 0)}</div></div>
      </div>`;
  } else {
    contentHTML = `[${esc(m.type)}消息]`;
  }
  let quoteHTML = "";
  if (m.reply_to) {
    const orig = (S.messages[c.id] || []).find((x) => x.seq === m.reply_to.seq);
    const q = orig ? (orig.type === "text" ? bodyText(orig) : `[${orig.type}]`) : `第 ${m.reply_to.seq} 条消息`;
    const qn = orig && orig.sender !== self ? displayNameOf(orig.sender) : orig ? "我" : "";
    quoteHTML = `<div class="msg-quote">${qn ? esc(qn) + "：" : ""}${esc(q).slice(0, 60)}</div>`;
  }
  let reactionsHTML = "";
  if (m.reactions && m.reactions.length) {
    const KEY_EMOJI = { like: "👍", heart: "❤️" };
    reactionsHTML = `<div class="reactions">` + m.reactions.map((r) =>
      `<button class="react-chip ${r.reacted ? "mine" : ""}" data-act="toggle-react" data-seq="${m.seq}" data-key="${esc(r.key)}" data-reacted="${!!r.reacted}">${esc(KEY_EMOJI[r.key] || r.key)} ${r.count > 1 ? r.count : ""}</button>`
    ).join("") + `</div>`;
  }
  const readTag = !isGroup && isSelf && isLastSelf && c.peer_read_seq >= m.seq
    ? `<span class="read-tag">已读</span>` : "";
  const menuBtn = `<button class="msg-menu-btn" data-act="msg-menu" data-seq="${m.seq}">⋯</button>`;
  return `
  <div class="msg-row ${isSelf ? "self" : ""}" data-seq="${m.seq}">
    ${avHTML(isSelf ? (S.me.nickname || self) : displayNameOf(m.sender))}
    <div class="msg-main">
      ${isGroup && !isSelf ? `<div class="msg-sender">${senderName}</div>` : ""}
      ${quoteHTML}
      <div class="msg-bubble">${contentHTML}</div>
      ${reactionsHTML}
      <div class="msg-meta"><span>${esc(fmtChatTime(m.created_at))}</span>${readTag}</div>
    </div>
    ${menuBtn}
  </div>`;
}

function bodyText(m) {
  const b = m.body;
  if (b == null) return "";
  if (typeof b === "string") { try { return JSON.parse(b).text || ""; } catch { return ""; } }
  return b.text || "";
}

function tipText(m) {
  const b = m.body;
  if (b && typeof b === "object" && b.text) return b.text;
  if (typeof b === "string") { try { return JSON.parse(b).text || "[系统消息]"; } catch { return "[系统消息]"; } }
  return "[系统消息]";
}

export function renderReplyBar() {
  const slot = $("#reply-bar-slot");
  if (!slot) return;
  if (!S.replyTo) { slot.innerHTML = ""; return; }
  const c = currentConv();
  const orig = (S.messages[c.id] || []).find((x) => x.seq === S.replyTo);
  const preview = orig ? (orig.type === "text" ? bodyText(orig) : `[${orig.type}]`) : "";
  const who = orig && orig.sender !== (S.me && S.me.username) ? displayNameOf(orig.sender) : "我";
  slot.innerHTML = `
    <div class="reply-bar">回复 ${esc(who)}：
      <span class="quote">${esc(preview).slice(0, 40)}</span>
      <button data-act="reply-cancel">✕</button>
    </div>`;
}

function bindChatInput() {
  const input = $("#chat-input");
  if (!input) return;
  let lastTyping = 0;
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); sendCurrentText(); }
  });
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
    const now = Date.now();
    if (now - lastTyping > 3000) {
      lastTyping = now;
      // 伪会话（peer:用户名）优先用会话列表里的真实 ID：对端才能把 typing 对到会话行上。
      // 列表里还没有会话时退回 toUser——此时对端也无会话行可显示，服务端 push 不带会话 ID 无影响。
      const cv = String(S.openConv || "");
      if (cv.startsWith("peer:")) {
        const peer = cv.slice(5);
        const known = S.convs.find((c) => c.type !== "group" && c.peer === peer);
        bridge("typing", known ? { id: known.id } : { toUser: peer }).catch(() => {});
      } else {
        bridge("typing", { id: cv }).catch(() => {});
      }
    }
  });
  setTimeout(() => input.focus(), 50);
}

export async function sendCurrentText() {
  const input = $("#chat-input");
  if (!input) return;
  const text = input.value.trim();
  if (!text || !S.openConv) return;
  const params = { id: S.openConv, text };
  if (S.replyTo) params.reply_seq = S.replyTo;
  try {
    $("#send-btn").disabled = true;
    const m = await bridge("send_text", params);
    input.value = ""; input.style.height = "auto";
    S.replyTo = null; renderReplyBar();
    input.focus();
    // 伪会话首条消息已送达：切到真实会话（走 openConv 重建 convMeta 并拉取历史，
    // 不能用伪会话数组直接覆盖真实会话键——msgs 推送可能先于 reply 到达）
    if (m && m.conversation_id && m.conversation_id !== S.openConv) {
      S.replyTo = null; renderReplyBar();
      await openConv(m.conversation_id);
    }
  } catch (err) {
    toast(err.message, true);
  } finally {
    const b = $("#send-btn"); if (b) b.disabled = false;
  }
}

export async function markRead() {
  const c = currentConv();
  if (!c || !document.hasFocus()) return;
  if (c.unread > 0 || c.marked_unread) {
    try { await bridge("read", { id: c.id, seq: c.max_seq }); } catch {}
  }
}

export async function openConv(id, preloaded) {
  S.openConv = id;
  S.replyTo = null;
  document.body.classList.add("show-main");
  if (String(id).startsWith("peer:")) { await openPseudoConv(String(id).slice(5)); return; }
  renderChatSkeleton();
  try {
    const data = preloaded || await bridge("open_conv", { id });
    S.messages[id] = data.messages || [];
    S.historyPages[id] = { has_more: data.has_more, next_before_seq: data.next_before_seq };
    S.convMeta[id] = { friend: data.friend || null, group: data.group || null, conversation: data.conversation };
    if (S.openConv !== id) return;
    renderChat();
    renderChatSub();
    markRead();
  } catch (err) {
    $("#pane-main").innerHTML = `<div class="main-empty">${esc(err.message)}</div>`;
  }
}

export async function loadOlderMessages() {
  const cid = S.openConv, page = S.historyPages[cid];
  if (!page?.has_more || page.loading || page.next_before_seq == null) return;
  page.loading = true;
  const body = $("#chat-body"), oldHeight = body?.scrollHeight || 0, oldTop = body?.scrollTop || 0;
  try {
    const data = await bridge("history", { id: cid, before_seq: page.next_before_seq });
    if (S.historyPages[cid] !== page) return;
    const messages = S.messages[cid] || (S.messages[cid] = []);
    for (const message of data.messages || []) upsertMessage(messages, message);
    S.historyPages[cid] = { has_more: data.has_more, next_before_seq: data.next_before_seq };
    if (S.openConv === cid) {
      renderMessages();
      const currentBody = $("#chat-body");
      if (currentBody) currentBody.scrollTop = oldTop + currentBody.scrollHeight - oldHeight;
    }
  } finally { page.loading = false; }
}

function renderChatSkeleton() {
  $("#pane-main").innerHTML = `
  <div class="chat">
    <div class="chat-head"><div class="chat-title">…</div></div>
    <div class="chat-body"><div class="chat-loading">加载中…</div></div>
    <div class="chat-foot"><div class="chat-input-area"><textarea class="chat-input" disabled></textarea></div></div>
  </div>`;
}

export function openPseudoConv(username) {
  S.openConv = "peer:" + username;
  S.replyTo = null;
  if (!S.messages[S.openConv]) S.messages[S.openConv] = [];
  renderChat();
}

export async function uploadAndSend(file, msgType) {
  const fd = new FormData();
  fd.append("file", file);
  const resp = await fetch("/api/upload", { method: "POST", body: fd });
  const data = await resp.json();
  if (!resp.ok) throw new Error(data.error || "上传失败");
  await bridge("send_media", { id: S.openConv, msg_type: msgType, file: data.file_info });
}
