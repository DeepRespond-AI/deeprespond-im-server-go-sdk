// 登录/注册表单与会话失效界面。
import { S } from "./state.js";
import { $ } from "./ui.js";

let loginMode = "login";

function setLoginMode(mode) {
  loginMode = mode;
  const btn = $("#login-btn"), sw = $("#login-switch"), errEl = $("#login-error");
  $("#login-nickname-field").classList.toggle("hidden", mode !== "register");
  btn.textContent = mode === "register" ? "注 册" : "登 录";
  sw.textContent = mode === "register" ? "已有账号？去登录" : "没有账号？注册一个";
  sw.dataset.mode = mode;
  errEl.classList.add("hidden");
}

export async function doLogin(onAuthenticated) {
  const btn = $("#login-btn");
  const errEl = $("#login-error");
  errEl.classList.add("hidden");
  const username = $("#login-username").value.trim();
  const password = $("#login-password").value;
  btn.disabled = true; btn.textContent = loginMode === "register" ? "注册中…" : "登录中…";
  try {
    if (loginMode === "register") {
      const regResp = await fetch("/api/register", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username, password,
          nickname: $("#login-nickname").value.trim(),
        }),
      });
      const regData = await regResp.json();
      if (!regResp.ok) throw new Error(regData.error || "注册失败");
    }
    const resp = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    const data = await resp.json();
    if (!resp.ok) throw new Error(data.error || "登录失败");
    S.me = data.me;
    S.kicked = false;
    $("#view-login").classList.add("hidden");
    $("#view-app").classList.remove("hidden");
    onAuthenticated();
  } catch (err) {
    errEl.textContent = err.message;
    errEl.classList.remove("hidden");
  } finally {
    btn.disabled = false; btn.textContent = loginMode === "register" ? "注 册" : "登 录";
  }
}

export function showLoginView() {
  $("#view-login").classList.remove("hidden");
  $("#view-app").classList.add("hidden");
}

export function showKicked(reason) {
  S.kicked = true;
  $("#kicked-reason").textContent = reason || "登录状态已失效";
  $("#kicked").classList.remove("hidden");
}

export function toggleLoginMode() { setLoginMode(loginMode === "login" ? "register" : "login"); }
