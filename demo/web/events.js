// 后端推送更新状态并刷新相关界面。
import { showKicked } from "./auth.js";
import { markRead, openConv, renderChatSub, renderMessages, renderReplyBar } from "./chat.js";
import { renderAll, renderConvList, renderListPane, renderMain, updateBadges } from "./render.js";
import { S, upsertMessage } from "./state.js";
import { $ } from "./ui.js";

export function setConn(state) {
  S.conn = state;
  const dot = $("#conn-dot");
  dot.classList.remove("on", "off", "mid");
  dot.classList.add(state === "connected" ? "on" : state === "connecting" ? "mid" : "off");
  if (S.tab === "me") renderMain();
}

function sweepTyping(force) {
  const now = Date.now();
  let dirty = false;
  for (const cid in S.typing) {
    const t = S.typing[cid];
    if (now - t.ts >= 5000 || now - (t.firstSeen || t.ts) >= 15000) { delete S.typing[cid]; dirty = true; }
  }
  if (dirty || force) { renderConvList(); renderChatSub(); }
}

export function handlePush(event, data) {
  switch (event) {
    case "bootstrap": {
      S.booted = true;
      $("#view-login").classList.add("hidden");
      $("#view-app").classList.remove("hidden");
      S.me = data.me; S.selfSession = data.self_session || "";
      S.messages = {}; S.historyPages = {}; S.convMeta = {};
      S.convs = data.conversations || [];
      S.friends = data.friends || [];
      S.requests = data.requests || { received: [], sent: [], unread: 0 };
      S.groups = data.groups || [];
      S.invitations = data.invitations || [];
      S.blacklist = data.blacklist || [];
      S.presence = {};
      for (const p of data.presence || []) S.presence[p.username] = p.state;
      renderAll();
      if (S.openConv && !String(S.openConv).startsWith("peer:") && S.convs.some((c) => c.id === S.openConv)) {
        openConv(S.openConv);
      }
      break;
    }
    case "convs": {
      S.convs = data || [];
      renderConvList(); updateBadges();
      // convs 推送可能晚于 msgs 到达（unread 刚变 1），补一次已读上报，否则对方永远看不到"已读"。
      if (S.openConv && !String(S.openConv).startsWith("peer:")) markRead();
      break;
    }
    case "msgs": {
      const cid = data.conversation_id;
      const arr = S.messages[cid] || (S.messages[cid] = []);
      for (const m of data.messages || []) upsertMessage(arr, m);
      // 对端消息已到达即视为对方停止输入：立即清掉 typing 指示器，不等现象窗口过期
      const msgsArr = data.messages || [];
      const lastMsg = msgsArr.length ? msgsArr[msgsArr.length - 1] : null;
      if (lastMsg && lastMsg.sender !== (S.me && S.me.username) && S.typing[cid]) {
        delete S.typing[cid];
        renderConvList();
        if (cid === S.openConv) renderChatSub();
      }
      if (cid === S.openConv) { renderMessages(); markRead(); }
      break;
    }
    case "msgs_deleted": {
      const arr = S.messages[data.conversation_id];
      if (arr) {
        S.messages[data.conversation_id] = arr.filter((m) => !data.seqs.includes(m.seq));
        if (data.conversation_id === S.openConv) renderMessages();
      }
      break;
    }
    case "msgs_reset": {
      S.messages[data.conversation_id] = [];
      delete S.historyPages[data.conversation_id];
      if (data.conversation_id === S.openConv) { S.replyTo = null; renderMessages(); renderReplyBar(); }
      break;
    }
    case "msgs_changed": {
      // message.changed 补丁：按 change.kind 把变化的字段合并进对应 seq 的消息。
      const arr = S.messages[data.conversation_id];
      const m = arr && arr.find((x) => x.seq === data.seq);
      if (m) {
        for (const key of ["body", "type", "ext", "mentions", "reply_to", "recalled", "edited", "pinned", "reactions", "erased", "message_version"]) {
          if (Object.hasOwn(data, key)) m[key] = data[key];
        }
        if (data.conversation_id === S.openConv) renderMessages();
      }
      break;
    }
    case "typing": {
      const now = Date.now();
      // firstSeen 记录本轮输入的起点：连续输入只续 ts 不续 firstSeen，使 15 秒硬上限可生效
      const prevTyping = S.typing[data.conversation_id];
      const firstSeen = prevTyping && now - prevTyping.firstSeen < 15000 ? prevTyping.firstSeen : now;
      S.typing[data.conversation_id] = { username: data.username, ts: now, firstSeen };
      renderConvList();
      if (data.conversation_id === S.openConv) renderChatSub();
      setTimeout(() => {
        const t = S.typing[data.conversation_id];
        if (t && Date.now() - t.ts >= 4900) {
          delete S.typing[data.conversation_id];
          renderConvList();
          if (data.conversation_id === S.openConv) renderChatSub();
        }
      }, 5000);
      break;
    }
    case "peer_read": {
      const c = S.convs.find((x) => x.id === data.conversation_id);
      if (c) { c.peer_read_seq = data.read_seq; renderMessages(); }
      break;
    }
    case "friends": S.friends = data || []; renderListPane(); break;
    case "requests": S.requests = data || { received: [], sent: [], unread: 0 }; renderListPane(); updateBadges(); break;
    case "groups": S.groups = data || []; renderListPane(); break;
    case "invitations": S.invitations = data || []; renderListPane(); updateBadges(); break;
    case "blacklist": S.blacklist = data || []; if (S.tab === "contacts") renderListPane(); break;
    case "presence":
      for (const p of data || []) S.presence[p.username] = p.state;
      renderListPane(); break;
    case "profile": S.me = data; renderMain(); break;
    case "conn": setConn(data.state); break;
    case "kicked": showKicked(data.reason); break;
  }
}

export function bindPushEvents() {
  setInterval(() => sweepTyping(false), 1000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") sweepTyping(true);
  });
}
