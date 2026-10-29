// 页面入口与用户操作；组合通信、状态和界面模块。
import { doLogin, showLoginView, toggleLoginMode } from "./auth.js";
import { bridge, connectBridge } from "./bridge.js";
import { loadOlderMessages, markRead, openConv, openPseudoConv, renderChat, renderReplyBar, sendCurrentText, uploadAndSend } from "./chat.js";
import { bindPushEvents, handlePush, setConn } from "./events.js";
import { loadSessions, renderGroupDetail, renderListPane, renderMain, updateBadges } from "./render.js";
import { S, currentConv } from "./state.js";
import { $, $$, avHTML, confirmDialog, displayNameOf, esc, friendCheckDialog, hideMenu, promptDialog, showErr, showMenu, toast } from "./ui.js";

document.addEventListener("click", async (e) => {
  if (!e.target.closest("#menu")) hideMenu();
  const t = e.target.closest("[data-act]");
  if (!t) return;
  const act = t.dataset.act;
  const d = t.dataset;
  try {
    switch (act) {
      // 导航
      case "nav-tab": {
        S.tab = d.tab;
        S.detail = null;
        renderListPane(); renderMain();
        if (d.tab === "chat" && S.openConv) { renderChat(); }
        // 「我的」页内容渲染在主面板：窄视口主面板默认隐藏，需展开才能看到（返回箭头回列表）。
        if (d.tab === "me" && innerWidth < 720) document.body.classList.add("show-main");
        else document.body.classList.remove("show-main");
        break;
      }
      case "back": {
        document.body.classList.remove("show-main");
        S.openConv = null; S.detail = null;
        // 窄视口从「我的」返回：切回聊天列表，避免列表停留在「我的」的加载占位。
        if (S.tab === "me") { S.tab = "chat"; renderListPane(); }
        renderMain();
        break;
      }

      // 会话
      case "conv-open": openConv(d.id); break;
      case "load-history": await loadOlderMessages(); break;
      case "conv-menu": {
        const c = S.convs.find((x) => x.id === d.id);
        showMenu([
          { label: c.pinned ? "取消置顶" : "置顶", onClick: () => bridge("pin_conv", { id: d.id, pinned: c.pinned ? "false" : "true" }).catch(showErr) },
          { label: "清空聊天记录", onClick: async () => { if (await confirmDialog("清空聊天记录", "本端不再展示历史消息")) bridge("clear_conv", { id: d.id }).catch(showErr); } },
          { label: "删除会话", danger: true, onClick: async () => { if (await confirmDialog("删除会话", "会话将从列表移除")) { if (S.openConv === d.id) { S.openConv = null; renderMain(); document.body.classList.remove("show-main"); } bridge("delete_conv", { id: d.id }).catch(showErr); } } },
        ], e.clientX, e.clientY);
        break;
      }
      case "group-info": {
        S.tab = "groups";
        S.detail = { kind: "group", data: { group_id: d.gid } };
        S.members = {};
        renderListPane(); renderGroupDetail();
        document.body.classList.add("show-main");
        break;
      }

      // 消息
      case "send": sendCurrentText(); break;
      case "pick-image": $("#file-image").value = ""; $("#file-image").click(); break;
      case "pick-file": $("#file-any").value = ""; $("#file-any").click(); break;
      case "open-file": window.open(d.url); break;
      case "msg-menu": {
        const c = currentConv();
        const m = (S.messages[c.id] || []).find((x) => x.seq === +d.seq);
        if (!m) break;
        const self = S.me && S.me.username;
        const items = [
          { label: "回复", onClick: () => { S.replyTo = m.seq; renderReplyBar(); $("#chat-input") && $("#chat-input").focus(); } },
          // 服务端 reaction key 只允许字母数字与 _-.:（emoji 会被 invalid_reaction_key 拒绝），用固定 key 传输、展示层映射 emoji。
          { label: "回应 👍", onClick: () => bridge("react", { id: c.id, seq: m.seq, key: "like" }).catch(showErr) },
          { label: "回应 ❤️", onClick: () => bridge("react", { id: c.id, seq: m.seq, key: "heart" }).catch(showErr) },
        ];
        if (m.sender === self) {
          items.push({ label: "撤回", danger: true, onClick: () => bridge("recall", { id: c.id, seq: m.seq }).catch(showErr) });
          items.push({ label: "删除", danger: true, onClick: () => bridge("delete_msgs", { id: c.id, seqs: [m.seq] }).catch(showErr) });
        }
        const r = t.getBoundingClientRect();
        showMenu(items, r.left, r.bottom);
        break;
      }
      case "toggle-react": {
        const c = currentConv();
        const params = { id: c.id, seq: +d.seq, key: d.key };
        if (d.reacted === "true") { params.remove = "true"; await bridge("react", params).catch(showErr); }
        else await bridge("react", params).catch(showErr);
        break;
      }
      case "reply-cancel": S.replyTo = null; renderReplyBar(); break;

      // 通讯录
      case "lookup-btn": {
        const name = $("#lookup-input").value.trim();
        const box = $("#lookup-result");
        if (!name) { box.innerHTML = ""; break; }
        try {
          const u = await bridge("lookup_user", { username: name });
          box.innerHTML = `
            <div class="row disabled">
              ${avHTML(u.nickname || u.username)}
              <div class="row-main">
                <div class="row-top"><span class="row-title">${esc(u.nickname || u.username)}</span></div>
                <div class="row-sub"><span class="preview">用户名：${esc(u.username)}</span></div>
              </div>
              <button class="btn-plain" data-act="add-friend" data-username="${esc(u.username)}">添加到通讯录</button>
            </div>`;
        } catch (err) {
          box.innerHTML = `<div class="empty-tip" style="padding:14px;color:var(--red)">${esc(err.message)}</div>`;
        }
        break;
      }
      case "add-friend": {
        if (!(await confirmDialog("添加好友", `将 ${displayNameOf(d.username)} 添加到通讯录？`))) break;
        await bridge("add_friend", { username: d.username });
        toast("已添加到通讯录");
        $("#lookup-result").innerHTML = "";
        break;
      }
      case "friend-open": {
        const f = S.friends.find((x) => x.username === d.username);
        if (!f) break;
        S.detail = { kind: "friend", data: f };
        renderListPane(); renderMain();
        document.body.classList.add("show-main");
        break;
      }
      case "friend-msg": {
        const res = await bridge("open_peer", { username: d.username });
        if (res.conversation && res.conversation.conversation_id) {
          S.convs = S.convs.some((x) => x.id === res.conversation.conversation_id)
            ? S.convs : [convFromState(res.conversation), ...S.convs];
          S.tab = "chat";
          renderListPane(); updateBadges();
          await openConv(res.conversation.conversation_id, res);
        } else {
          // 尚无往来：打开伪会话，首条消息发出后服务端建会话
          S.tab = "chat";
          renderListPane();
          await openPseudoConv(d.username);
        }
        break;
      }
      case "friend-remark": {
        const remark = await promptDialog("设置好友备注", "备注名", d.remark || "");
        if (remark === null) break;
        await bridge("remark", { username: d.username, remark });
        toast("备注已保存");
        break;
      }
      case "friend-remove": {
        if (!(await confirmDialog("删除好友", `确定删除 ${displayNameOf(d.username)} 吗？`))) break;
        await bridge("remove_friend", { username: d.username });
        S.detail = null;
        renderListPane(); renderMain();
        document.body.classList.remove("show-main");
        toast("已删除好友");
        break;
      }
      case "friend-block": {
        if (!(await confirmDialog("加入黑名单", `拉黑后 ${displayNameOf(d.username)} 将无法给你发消息`))) break;
        await bridge("block", { username: d.username });
        toast("已加入黑名单");
        break;
      }
      case "bl-remove":
        await bridge("unblock", { username: d.username });
        toast("已移出黑名单");
        break;
      case "req-accept": await bridge("accept_friend", { username: d.username }); toast("已成为好友"); break;
      case "req-decline": await bridge("decline_friend", { username: d.username }); break;

      // 群
      case "group-open": {
        const g = S.groups.find((x) => x.group_id === d.gid);
        if (!g) break;
        S.detail = { kind: "group", data: g };
        S.members = {};
        renderListPane(); renderGroupDetail();
        document.body.classList.add("show-main");
        break;
      }
      case "group-create":
        friendCheckDialog("创建群聊", async (picked) => {
          const name = await promptDialog("群聊名称", "输入群名");
          if (!name) return;
          const g = await bridge("create_group", { name, members: picked });
          toast("群聊已创建");
          await groupOpenAndChat(g.group_id);
        });
        break;
      case "inv-accept": await bridge("accept_invitation", { invitation_id: d.iid }); toast("已加入群聊"); break;
      case "inv-decline": await bridge("decline_invitation", { invitation_id: d.iid }); break;
      case "group-msg": await groupOpenAndChat(d.gid); break;
      case "group-online": {
        const r = await bridge("online_count", { group_id: d.gid });
        toast(`当前在线 ${r.online_count} 人`);
        break;
      }
      case "group-rename": {
        const name = await promptDialog("修改群名", "输入新群名", d.name || "");
        if (!name) break;
        await bridge("rename_group", { id: d.gid, name });
        toast("群名已更新");
        break;
      }
      case "group-invite":
        friendCheckDialog("邀请好友进群", async (picked) => {
          if (!picked.length) return;
          const response = await bridge("invite_group", { group_id: d.gid, users: picked });
          const labels = { joined: "已加入群聊", invited: "已发送邀请，等待对方确认", pending_review: "已提交，等待管理员审核", already_member: "已在群中" };
          const text = (response.results || []).map((r) => `${r.username}：${r.message || r.code || labels[r.status] || "邀请未完成"}`).join("\n");
          alert(text || "未返回邀请结果，请刷新群成员后重试");
          S.members[d.gid] = null;
          renderGroupDetail();
        });
        break;
      case "group-leave": {
        if (!(await confirmDialog("退出群聊", "退出后将不再接收该群消息"))) break;
        await bridge("leave_group", { group_id: d.gid });
        S.detail = null;
        renderListPane(); renderMain();
        document.body.classList.remove("show-main");
        toast("已退出群聊");
        break;
      }

      // 我的
      case "me-edit-nick": {
        const nick = await promptDialog("修改昵称", "输入新昵称", (S.me.nickname || ""));
        if (!nick) break;
        await bridge("update_profile", { nickname: nick });
        toast("昵称已更新");
        break;
      }
      case "pw-submit": {
        const msgEl = $("#pw-msg");
        msgEl.classList.add("hidden");
        try {
          await bridge("set_password", { old: $("#pw-old").value, new: $("#pw-new").value });
          msgEl.className = "form-ok"; msgEl.textContent = "密码修改成功";
          msgEl.classList.remove("hidden");
          $("#pw-old").value = ""; $("#pw-new").value = "";
        } catch (err) {
          msgEl.className = "form-err"; msgEl.textContent = err.message;
          msgEl.classList.remove("hidden");
        }
        break;
      }
      case "sessions-refresh": loadSessions(); break;
      case "session-revoke": {
        const cur = t.classList.contains("danger");
        const ok = await confirmDialog(cur ? "下线本机" : "下线该设备", cur ? "下线后本会话将立即失效，需要重新登录" : "该设备的登录状态将被吊销");
        if (!ok) break;
        await bridge("revoke_session", { session_id: d.sid });
        if (!cur) { toast("已下线该设备"); loadSessions(); }
        break;
      }
      case "me-logout": {
        if (!(await confirmDialog("退出登录", "确定要退出当前账号吗？"))) break;
        await fetch("/api/logout", { method: "POST" });
        location.reload();
        break;
      }
      case "kicked-relogin": location.reload(); break;
    }
  } catch (err) {
    showErr(err);
  }
});

function convFromState(st) {
  return {
    id: st.conversation_id, type: st.conversation_type, peer: st.peer, group_id: st.group_id,
    name: st.peer || "", unread: st.unread_count, pinned: !!st.pinned_at, marked_unread: st.marked_unread,
    max_seq: st.max_seq, last_seq: st.max_seq, last_at: st.last_message_at || "",
    last_preview: "", peer_read_seq: st.peer_read_seq,
  };
}

async function groupOpenAndChat(gid) {
  const conv = S.convs.find((c) => c.type === "group" && c.group_id === gid);
  S.tab = "chat";
  renderListPane(); updateBadges();
  if (conv) await openConv(conv.id);
  else toast("群会话同步中，请稍后从聊天列表打开");
}

function checkViewport() {
  if (innerWidth >= 720) document.body.classList.remove("show-main");
}

function startBridge() {
  connectBridge(handlePush, () => {
    if (S.kicked) return;
    setConn("disconnected");
    if (!S.booted) showLoginView();
    else setTimeout(startBridge, 2000);
  });
}

$("#file-image").addEventListener("change", (event) => {
  const file = event.target.files[0];
  if (file && S.openConv) uploadAndSend(file, "image").catch(showErr);
});
$("#file-any").addEventListener("change", (event) => {
  const file = event.target.files[0];
  if (file && S.openConv) uploadAndSend(file, "file").catch(showErr);
});
$("#login-btn").addEventListener("click", () => doLogin(startBridge));
$("#login-switch").addEventListener("click", toggleLoginMode);
$$("#view-login input").forEach((input) => input.addEventListener("keydown", (event) => {
  if (event.key === "Enter") doLogin(startBridge);
}));
window.addEventListener("resize", checkViewport);
window.addEventListener("focus", () => { if (S.openConv) markRead(); });
bindPushEvents();
startBridge();
