// 格式化、头像、提示、对话框和菜单。
import { S } from "./state.js";

export const $ = (sel, el = document) => el.querySelector(sel);
export const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];


export function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (m) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[m]));
}

const AV_COLORS = ["#4f7ded", "#07c160", "#fa9d3b", "#f76260", "#8f6bf5", "#00b8a9", "#5b8cff", "#e0a03c"];

function avColor(name) {
  let h = 2166136261;
  for (const ch of String(name)) { h ^= ch.charCodeAt(0); h = Math.imul(h, 16777619) >>> 0; }
  return AV_COLORS[h % AV_COLORS.length];
}

export function avHTML(name, cls = "", showPresence = false) {
  const initial = String(name || "?").slice(0, 1).toUpperCase();
  const dot = showPresence
    ? `<span class="presence-dot ${S.presence[name] === "online" ? "online" : ""}"></span>` : "";
  return `<div class="avatar ${cls}" style="--c:${avColor(name)}">${esc(initial)}${dot}</div>`;
}

export function displayNameOf(username) {
  if (username === (S.me && S.me.username)) return (S.me.nickname || username);
  const f = S.friends.find((x) => x.username === username);
  return (f && (f.remark || f.nickname)) || username;
}

export function fmtSize(n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}

export function fmtListTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hh = String(d.getHours()).padStart(2, "0"), mm = String(d.getMinutes()).padStart(2, "0");
  if (sameDay) return `${hh}:${mm}`;
  const yest = new Date(now); yest.setDate(now.getDate() - 1);
  if (d.toDateString() === yest.toDateString()) return "昨天";
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

export function fmtChatTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getMonth() + 1}月${d.getDate()}日 ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function b64url(s) {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function mediaSrc(u, ct) {
  if (!u) return "";
  return `/api/media?u=${encodeURIComponent(b64url(u))}&ct=${encodeURIComponent(ct || "")}`;
}

export function toast(msg, isErr = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.classList.toggle("err", isErr);
  el.classList.remove("hidden");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.add("hidden"), 2400);
}

export function showErr(err) { toast(err.message || String(err), true); }

function showDialog(html) {
  const ov = $("#overlay");
  ov.innerHTML = `<div class="dialog">${html}</div>`;
  ov.classList.remove("hidden");
  ov.onclick = (e) => { if (e.target === ov) closeDialog(); };
  return ov;
}

function closeDialog() {
  const ov = $("#overlay");
  ov.classList.add("hidden");
  ov.innerHTML = "";
}

export function promptDialog(title, placeholder, value = "") {
  return new Promise((resolve) => {
    showDialog(`
      <h3>${esc(title)}</h3>
      <div class="dialog-body">
        <input type="text" id="dlg-input" placeholder="${esc(placeholder)}" value="${esc(value)}">
      </div>
      <div class="dialog-foot">
        <button class="dlg-cancel" data-dlg="cancel">取消</button>
        <button class="dlg-ok" data-dlg="ok">确定</button>
      </div>`);
    const input = $("#dlg-input");
    input.focus(); input.select();
    input.onkeydown = (e) => { if (e.key === "Enter") { resolve(input.value.trim()); closeDialog(); } };
    $$("#overlay [data-dlg]").forEach((b) => b.onclick = () => {
      resolve(b.dataset.dlg === "ok" ? input.value.trim() : null);
      closeDialog();
    });
  });
}

export function confirmDialog(title, text) {
  return new Promise((resolve) => {
    showDialog(`
      <h3>${esc(title)}</h3>
      <div class="dialog-body" style="text-align:center;color:var(--text-2);font-size:13px">${esc(text)}</div>
      <div class="dialog-foot">
        <button class="dlg-cancel" data-dlg="cancel">取消</button>
        <button class="dlg-ok" data-dlg="ok" style="background:var(--red)">确定</button>
      </div>`);
    $$("#overlay [data-dlg]").forEach((b) => b.onclick = () => {
      resolve(b.dataset.dlg === "ok"); closeDialog();
    });
  });
}

export function friendCheckDialog(title, onOk) {
  const rows = S.friends.map((f) => `
    <label class="check-row">
      <input type="checkbox" value="${esc(f.username)}">
      ${avHTML(f.remark || f.nickname || f.username, "sm")}
      <span>${esc(f.remark || f.nickname || f.username)}</span>
    </label>`).join("");
  showDialog(`
    <h3>${esc(title)}</h3>
    <div class="dialog-body">
      ${rows || `<div class="empty-tip">还没有好友</div>`}
    </div>
    <div class="dialog-foot">
      <button class="dlg-cancel" data-dlg="cancel">取消</button>
      <button class="dlg-ok" data-dlg="ok">确定</button>
    </div>`);
  $$("#overlay [data-dlg]").forEach((b) => b.onclick = () => {
    // 先关弹窗再回调：onOk 里可能再弹新对话框（如创建群聊 → 输入群名），
    // 否则外层 closeDialog 会把新弹的对话框关掉。
    const ok = b.dataset.dlg === "ok";
    const picked = ok ? $$("#overlay input:checked").map((i) => i.value) : null;
    closeDialog();
    if (ok) onOk(picked);
  });
}

export function showMenu(items, x, y) {
  const menu = $("#menu");
  menu.innerHTML = items.map((it, i) =>
    `<button class="menu-item ${it.danger ? "danger" : ""}" data-mi="${i}">${esc(it.label)}</button>`).join("");
  menu.classList.remove("hidden");
  const mw = 150, mh = items.length * 38 + 12;
  menu.style.left = Math.min(x, innerWidth - mw - 8) + "px";
  menu.style.top = Math.min(y, innerHeight - mh - 8) + "px";
  $$("#menu [data-mi]").forEach((b) => b.onclick = () => {
    hideMenu();
    items[+b.dataset.mi].onClick();
  });
}

export function hideMenu() { $("#menu").classList.add("hidden"); }
