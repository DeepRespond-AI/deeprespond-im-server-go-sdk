// Go 服务端 SDK Demo E2E：Playwright 双浏览器（双账号）真实用户流程 + 关键失败路径。
// 用法：node e2e.mjs <desktop|narrow>
// 每步输出一行 E2E|{json}（操作/预期/实际/通过/截图路径），并按编号保存截图。
// 断言全部基于 DOM（推送到达后界面会重渲染），不依赖页面内部状态对象。
import { createRequire } from "node:module";
import { mkdirSync, writeFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || "playwright");

const MODE = process.argv[2] || "desktop";
const VIEWPORTS = {
  desktop: { width: 1280, height: 900, dir: "截图-E2E-桌面" },
  narrow: { width: 375, height: 812, dir: "截图-E2E-窄视口" },
};
const vp = VIEWPORTS[MODE];
if (!vp) { console.error("模式必须是 desktop 或 narrow"); process.exit(2); }

const BASE = process.env.IM_DEMO_TEST_URL || "http://127.0.0.1:18392";
const CONTROL = process.env.IM_TEST_HARNESS || "http://127.0.0.1:18890";
const OUT_DIR = process.env.E2E_OUTPUT_DIR || join(here, "../../../../../artifacts/server-go-demo-e2e");
const SHOT_DIR = join(OUT_DIR, vp.dir);
mkdirSync(SHOT_DIR, { recursive: true });
writeFileSync(join(OUT_DIR, `e2e-results-${MODE}.json`), "");

const rand = Math.random().toString(36).slice(2, 8);
const A = { username: `e2ego_a_${rand}`, password: "Pass-e2e-1", nickname: "小艾" };
const B = { username: `e2ego_b_${rand}`, password: "Pass-e2e-1", nickname: "小碧" };
const GROUP_NAME = `Go小队${rand.slice(0, 3)}`;
const PNG_B64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitFor(fn, timeout = 12000, desc = "") {
  const t0 = Date.now();
  let lastErr = null;
  while (Date.now() - t0 < timeout) {
    try { const v = await fn(); if (v) return v; } catch (e) { lastErr = e; }
    await sleep(200);
  }
  throw new Error(`等待超时(${timeout}ms) ${desc}${lastErr ? `：${lastErr.message}` : ""}`);
}

const results = [];
const browserErrors = [];
function watch(page) { page.on("pageerror", error => browserErrors.push(error.message)); }
let shotPage = null;
async function step(no, name, action, expect, fn, page) {
  shotPage = page || shotPage;
  const shot = `${String(no).padStart(2, "0")}-${name}.png`;
  let pass = false, actual = "";
  try {
    const detail = await fn();
    actual = detail === undefined || detail === "" ? "符合预期" : String(detail);
    pass = true;
  } catch (e) {
    actual = `FAIL: ${e.message}`;
  }
  try { await shotPage.screenshot({ path: join(SHOT_DIR, shot) }); }
  catch (e) { pass = false; actual += `（截图失败：${e.message}）`; }
  const rec = { viewport: MODE, no, name, action, expect, actual, pass, shot: join(vp.dir, shot) };
  results.push(rec);
  console.log(`E2E|${JSON.stringify(rec)}`);
}

async function registerAndLogin(page, u) {
  await page.goto(BASE, { waitUntil: "networkidle" });
  await page.waitForSelector("#view-login", { state: "visible", timeout: 10000 });
  await page.click("#login-switch"); // 切到注册模式
  await page.fill("#login-nickname", u.nickname);
  await page.fill("#login-username", u.username);
  await page.fill("#login-password", u.password);
  await page.click("#login-btn");
  // 注册 + 登录成功 → 主界面可见；失败则读出错误提示
  try {
    await page.waitForSelector("#view-app", { state: "visible", timeout: 15000 });
    await page.waitForSelector("#pane-list .row, #pane-list .empty-tip", { timeout: 15000 });
  } catch (e) {
    const err = await page.locator("#login-error").textContent().catch(() => "");
    throw new Error(err ? `注册/登录失败：${err.trim()}` : e.message);
  }
}

const navTab = (page, tab) => page.click(`.nav-btn[data-tab="${tab}"]`);
const convRow = (page, title) => page.locator(".conv-row", { hasText: title }).first();
async function openChatWith(page, title) {
  await convRow(page, title).click();
  await page.waitForSelector("#chat-input", { timeout: 8000 });
}
async function sendText(page, text) {
  await page.fill("#chat-input", text);
  await page.press("#chat-input", "Enter");
}

async function main() {
  const browser = await chromium.launch({
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}),
    headless: true,
    args: ["--no-sandbox"],
  });

  // ============ 用户 A：注册 ============
  const ctxA = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, deviceScaleFactor: 2 });
  const pa = await ctxA.newPage();
  watch(pa);
  shotPage = pa;
  await pa.goto(BASE, { waitUntil: "networkidle" });
  await pa.waitForSelector("#view-login", { state: "visible" });
  await step(1, "登录页", "打开 demo 首页", "显示登录卡片（含注册入口）",
    async () => `标题=${(await pa.locator("h1").first().textContent())?.trim()}，注册入口=${await pa.locator("#login-switch").isVisible()}`, pa);

  await step(2, "注册用户A", `切换到注册模式，注册 ${A.username}（SDK users.create）`, "注册成功并自动登录进主界面", async () => {
    await registerAndLogin(pa, A);
    return `进入主界面，聊天窗标题=${await pa.locator(".list-head h2").first().textContent()}`;
  }, pa);

  await step(3, "空状态", "新用户无好友无会话", "聊天/通讯录显示空状态", async () => {
    const chatEmpty = (await pa.locator("#pane-list .empty-tip").first().textContent())?.trim();
    await navTab(pa, "contacts");
    await pa.waitForSelector("#pane-list .empty-tip", { timeout: 8000 });
    const contactEmpty = (await pa.locator("#pane-list .empty-tip").first().textContent())?.trim();
    await navTab(pa, "chat");
    return `聊天=「${chatEmpty?.slice(0, 10)}…」 通讯录=「${contactEmpty?.slice(0, 8)}…」`;
  }, pa);

  // ============ 用户 B：注册 ============
  const ctxB = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, deviceScaleFactor: 2 });
  const pb = await ctxB.newPage();
  watch(pb);
  await step(4, "注册用户B", `独立浏览器注册并登录 ${B.username}`, "进入主界面", async () => {
    await registerAndLogin(pb, B);
    return `进入主界面，聊天窗标题=${await pb.locator(".list-head h2").first().textContent()}`;
  }, pb);

  // ============ 好友 ============
  await step(5, "查找用户", `A 在通讯录查找 ${B.username}（users.get）`, "显示用户卡片和添加按钮", async () => {
    await navTab(pa, "contacts");
    await pa.fill("#lookup-input", B.username);
    await pa.click('[data-act="lookup-btn"]');
    await pa.waitForSelector("#lookup-result .row", { timeout: 8000 });
    return `找到「${(await pa.locator("#lookup-result .row-title").first().textContent())?.trim()}」`;
  }, pa);

  await step(6, "添加好友", "A 点击「添加到通讯录」并确认直接添加（friends.add）", "A 的好友列表出现 B", async () => {
    await pa.click('#lookup-result [data-act="add-friend"]');
    await pa.locator('#overlay h3', { hasText: "添加好友" }).waitFor();
    await pa.click('#overlay [data-dlg="ok"]');
    await pa.waitForSelector(".friend-row", { timeout: 10000 });
    return `A 的好友「${(await pa.locator(".friend-row .row-title").first().textContent())?.trim()}」已显示`;
  }, pa);

  await step(7, "对方实时收到好友", "B 等 friend.added 回调推送后打开通讯录", "B 的通讯录实时出现 A", async () => {
    await navTab(pb, "contacts");
    await pb.waitForSelector(`.friend-row:has-text("${A.nickname}")`, { timeout: 15000 });
    return `B 的好友列表含「${A.nickname}」`;
  }, pb);

  // ============ 单聊 ============
  await step(8, "发第一条单聊", "A 从好友详情给 B 发消息（伪会话 → messages.send）", "消息气泡出现在聊天窗", async () => {
    await pa.locator(".friend-row", { hasText: B.nickname }).click();
    await pa.click('[data-act="friend-msg"]');
    await pa.waitForSelector("#chat-input", { timeout: 8000 });
    await sendText(pa, "你好，我是 E2E 机器人");
    await pa.waitForSelector('.msg-bubble:has-text("你好，我是 E2E 机器人")', { timeout: 10000 });
    return "气泡已显示";
  }, pa);

  await step(9, "对方实时收到", "B 等 message.sent 回调推送", "B 的会话列表出现新会话与未读角标", async () => {
    await navTab(pb, "chat");
    await pb.waitForSelector(`.conv-row:has-text("${A.nickname}") .badge`, { timeout: 15000 });
    return `未读角标=${(await pb.locator(`.conv-row:has-text("${A.nickname}") .badge`).first().textContent())?.trim()}`;
  }, pb);

  await step(10, "阅读并回复", "B 打开会话（已读上报）并回复", "A 收到回复并看到「已读」", async () => {
    await openChatWith(pb, A.nickname);
    await sendText(pb, "收到，你好！");
    await pa.waitForSelector('.msg-bubble:has-text("收到，你好！")', { timeout: 15000 });
    await pa.waitForSelector(".read-tag", { timeout: 15000 });
    return "A 的气泡出现「已读」标记";
  }, pb);

  await step(11, "撤回消息", "A 在消息菜单点「撤回」（用户设备撤回接口）", "双方都显示撤回提示", async () => {
    const rowA = pa.locator(".msg-row", { hasText: "你好，我是 E2E 机器人" }).first();
    await rowA.hover(); // ⋯ 菜单按钮悬停才显示
    await rowA.locator(".msg-menu-btn").click();
    await pa.locator('#menu [data-mi]', { hasText: "撤回" }).click();
    await pb.waitForSelector('.msg-sys:has-text("撤回了一条消息")', { timeout: 15000 });
    await pa.waitForSelector('.msg-sys:has-text("撤回了一条消息")', { timeout: 15000 });
    return "双方显示「撤回了一条消息」";
  }, pa);

  // ============ 图片 ============
  await step(12, "发图片", "A 上传 PNG 并发送（media.upload + messages.send）", "B 的聊天窗渲染出图片", async () => {
    await pa.setInputFiles("#file-image", { name: "dot.png", mimeType: "image/png", buffer: Buffer.from(PNG_B64, "base64") });
    await pb.waitForSelector("img.msg-image", { timeout: 20000 });
    await pb.waitForFunction(() => [...document.querySelectorAll("img.msg-image")].some(image => image.complete && image.naturalWidth > 0));
    return "图片已在 B 的聊天窗渲染";
  }, pa);

  // ============ 群 ============
  await step(13, "创建群聊", `A 创建「${GROUP_NAME}」并选择 B（groups.create）`, "群会话打开", async () => {
    await navTab(pa, "groups");
    await pa.click('[data-act="group-create"]');
    await pa.locator('.check-row input[type="checkbox"]').first().check();
    await pa.click('#overlay [data-dlg="ok"]');
    await pa.fill("#dlg-input", GROUP_NAME);
    await pa.click('#overlay [data-dlg="ok"]');
    await navTab(pa, "chat");
    await pa.waitForSelector(`.conv-row:has-text("${GROUP_NAME}")`, { timeout: 15000 });
    await convRow(pa, GROUP_NAME).click();
    await pa.waitForSelector('#chat-title-text:has-text("' + GROUP_NAME + '")', { timeout: 10000 });
    return `群聊「${GROUP_NAME}」已创建并打开`;
  }, pa);

  await step(14, "成员实时进群", "B 等 group.created/members_added 回调", "B 的群组列表出现新群", async () => {
    await navTab(pb, "groups");
    await pb.waitForSelector(`.group-row:has-text("${GROUP_NAME}")`, { timeout: 15000 });
    return `B 的群列表含「${GROUP_NAME}」`;
  }, pb);

  await step(15, "群聊消息", "A 在群里发言，B 打开群会话确认收到", "双方群气泡正常", async () => {
    await sendText(pa, "欢迎大家来到群聊");
    await navTab(pb, "chat");
    await pb.waitForSelector(`.conv-row:has-text("${GROUP_NAME}")`, { timeout: 15000 });
    await convRow(pb, GROUP_NAME).click();
    await pb.waitForSelector("#chat-input", { timeout: 8000 });
    await pb.waitForSelector('.msg-bubble:has-text("欢迎大家来到群聊")', { timeout: 15000 });
    return "群消息双方可见";
  }, pb);

  await step(16, "群改名", "A 在群详情改名（groups.update）", "B 的会话列表群名实时更新", async () => {
    await navTab(pa, "groups");
    await pa.locator(".group-row", { hasText: GROUP_NAME }).click();
    await pa.waitForSelector('[data-act="group-rename"]', { timeout: 8000 });
    await pa.click('[data-act="group-rename"]');
    await pa.fill("#dlg-input", GROUP_NAME + "改");
    await pa.click('#overlay [data-dlg="ok"]');
    await navTab(pb, "chat");
    await pb.waitForSelector(`.conv-row:has-text("${GROUP_NAME}改")`, { timeout: 15000 });
    return `B 看到新群名「${GROUP_NAME}改」`;
  }, pa);

  // ============ 资料 / 设备 ============
  await step(17, "修改昵称", "A 修改自己的昵称（users.update）", "A 的资料更新，B 的会话列表显示名更新", async () => {
    await navTab(pa, "me");
    await pa.click('[data-act="me-edit-nick"]');
    await pa.fill("#dlg-input", "小艾改");
    await pa.click('#overlay [data-dlg="ok"]');
    await pa.waitForSelector('.me-card:has-text("小艾改")', { timeout: 10000 });
    await pa.locator("#sessions-box .me-row").first().waitFor();
    await navTab(pb, "chat");
    await pb.waitForSelector('.conv-row:has-text("小艾改")', { timeout: 15000 });
    return "B 的会话列表显示「小艾改」";
  }, pa);

  await step(18, "设备与登录态", "B 打开「我的」页查看设备列表（users.list_sessions）", "列出 Go Server Demo 设备", async () => {
    await navTab(pb, "me");
    await pb.click('[data-act="sessions-refresh"]');
    await pb.waitForSelector("#sessions-box .me-row", { timeout: 10000 });
    await pb.locator(".me-card", { hasText: "已连接" }).waitFor({ state: "visible" });
    const first = (await pb.locator("#sessions-box .me-row .k").first().textContent())?.trim();
    return `设备行=${await pb.locator("#sessions-box .me-row").count()}，首行=${first}`;
  }, pb);

  // ============ 失败路径 ============
  await step(19, "不支持的能力提示", "A 点消息菜单「回应」（服务端 OpenAPI 不提供）", "明确的错误提示而不是无响应", async () => {
    await navTab(pa, "chat");
    await openChatWith(pa, B.nickname);
    const lastRow = pa.locator(".msg-row").last();
    await lastRow.hover(); // ⋯ 菜单按钮悬停才显示
    await lastRow.locator(".msg-menu-btn").click();
    await pa.locator('#menu [data-mi]', { hasText: "回应" }).first().click();
    await pa.locator("#toast.err", { hasText: "OpenAPI" }).waitFor({ state: "visible", timeout: 8000 });
    return `提示=「${(await pa.locator("#toast").textContent())?.trim()}」`;
  }, pa);

  await step(20, "完整历史首页", "A 连续发送 32 条，B 重新打开单聊", "首次加载最近 30 条，出现历史翻页入口", async () => {
    for (let i = 1; i <= 32; i++) {
      await sendText(pa, `历史回归 ${i}`);
      await pa.locator(".msg-bubble", { hasText: new RegExp(`^历史回归 ${i}$`) }).waitFor();
    }
    await navTab(pb, "chat");
    await openChatWith(pb, "小艾改");
    await pb.locator('[data-act="load-history"]').waitFor();
    if (await pb.locator(".msg-row").count() !== 30) throw new Error("首页不是 30 条消息");
    return "首页 30 条，历史入口可见";
  }, pb);

  await step(21, "加载更早历史", "B 点击历史翻页按钮", "早期回复重新可见，消息数量增加", async () => {
    await pb.click('[data-act="load-history"]');
    await pb.locator(".msg-bubble", { hasText: /^收到，你好！$/ }).waitFor();
    if (await pb.locator(".msg-row").count() <= 30) throw new Error("历史没有增加");
    return `加载后消息行数=${await pb.locator(".msg-row").count()}`;
  }, pb);

  await step(22, "删除后刷新", "B 删除自己的早期回复，刷新并加载历史", "删除对 B 持久生效，A 仍可见", async () => {
    const row = pb.locator(".msg-row.self", { hasText: "收到，你好！" });
    await row.hover(); await row.locator(".msg-menu-btn").click();
    await pb.locator('#menu [data-mi]', { hasText: "删除" }).click();
    await pb.locator(".msg-bubble", { hasText: /^收到，你好！$/ }).waitFor({ state: "hidden" });
    await pb.reload(); await pb.locator("#view-app").waitFor({ state: "visible" });
    await openChatWith(pb, "小艾改");
    await pb.click('[data-act="load-history"]');
    await pb.locator('.msg-sys', { hasText: "撤回了一条消息" }).waitFor();
    if (await pb.locator(".msg-bubble", { hasText: /^收到，你好！$/ }).count()) throw new Error("删除的回复重新出现");
    if (!await pa.locator(".msg-bubble", { hasText: /^收到，你好！$/ }).count()) throw new Error("误删对方历史");
    return "刷新后已删除消息不可见，对方记录保留";
  }, pb);

  await step(23, "编辑回调正文", "经本机 harness 管理接口编辑最后一条消息", "双方消息正文经签名回调实时替换", async () => {
    async function admin(method, path, body) {
      const response = await fetch(`${CONTROL}/openapi`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ method, path, body }) });
      if (!response.ok) throw new Error(`管理接口 HTTP ${response.status}`);
      return response.json();
    }
    const conv = await admin("GET", `/single-conversations/${A.username}/${B.username}`);
    const page = await admin("GET", `/conversations/${conv.conversation_id}/messages?limit=1`);
    await admin("PATCH", `/messages/${page.items[0].message_id}`, { body: { text: "编辑回归：正文已同步" } });
    for (const page of [pa, pb]) await page.locator(".msg-bubble", { hasText: /^编辑回归：正文已同步$/ }).waitFor();
    return "双方均展示编辑后的正文";
  }, pb);

  await step(24, "清空后刷新", "B 清空单聊并刷新重新打开", "旧历史与翻页入口均消失", async () => {
    await navTab(pb, "chat");
    const row = convRow(pb, "小艾改");
    await row.hover(); await row.locator('[data-act="conv-menu"]').click();
    await pb.locator('#menu [data-mi]', { hasText: "清空聊天记录" }).click();
    await pb.click('#overlay [data-dlg="ok"]');
    await openChatWith(pb, "小艾改");
    await pb.locator("#chat-body .empty-tip").waitFor();
    await pb.reload(); await pb.locator("#view-app").waitFor({ state: "visible" });
    await openChatWith(pb, "小艾改");
    await pb.locator("#chat-body .empty-tip").waitFor();
    if (await pb.locator('[data-act="load-history"]').count()) throw new Error("仍有旧历史入口");
    return "刷新后仍为空记录";
  }, pb);

  await step(25, "HTML附件安全下载", "A 发 HTML 文件，B 用浏览器下载附件", "触发下载，脚本不在 Demo 同源执行", async () => {
    await pa.setInputFiles("#file-any", { name: "review.html", mimeType: "text/html", buffer: Buffer.from('<!doctype html><script>window.reviewProof=true;</script>') });
    const card = pb.locator('.file-card', { hasText: "review.html" });
    await card.waitFor();
    const url = await card.getAttribute("data-url");
    const download = pb.waitForEvent("download");
    await pb.evaluate(url => { window.location.href = url; }, url);
    const file = await download;
    if (file.suggestedFilename() !== "download" || await pb.evaluate(() => Boolean(window.reviewProof))) throw new Error("附件未安全下载");
    return `浏览器下载 ${file.suggestedFilename()}，未执行脚本`;
  }, pb);

  await step(26, "退出登录", "A 退出登录", "回到登录页", async () => {
    await navTab(pa, "me");
    await pa.click('[data-act="me-logout"]');
    await pa.waitForSelector('#overlay [data-dlg="ok"]', { timeout: 8000 }); // 退出确认对话框
    await pa.click('#overlay [data-dlg="ok"]');
    await pa.waitForSelector("#view-login", { state: "visible", timeout: 10000 });
    return "登录页已显示";
  }, pa);

  await step(27, "错误密码", "A 退出后使用错误密码登录", "显示明确错误，主界面不开放", async () => {
    await pa.fill("#login-username", A.username);
    await pa.fill("#login-password", "wrong-password");
    await pa.click("#login-btn");
    await pa.locator("#login-error", { hasText: "密码" }).waitFor({ state: "visible" });
    if (await pa.locator("#view-app").isVisible()) throw new Error("错误密码进入主界面");
    return "错误密码被拒绝";
  }, pa);
  await step(28, "模块与布局检查", "检查 JavaScript 错误及横向溢出", "无模块加载错误，桌面与手机均无横向溢出", async () => {
    if (browserErrors.length) throw new Error(browserErrors.join("; "));
    for (const page of [pa, pb]) {
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)) throw new Error("页面横向溢出");
    }
    return "JavaScript 错误 0，横向溢出 0";
  }, pb);
  await ctxB.request.post(`${BASE}/api/logout`);
  await browser.close();

  const failed = results.filter((r) => !r.pass);
  writeFileSync(join(OUT_DIR, `e2e-results-${MODE}.json`), JSON.stringify(results, null, 2));
  console.log(`# E2E(${MODE})：${results.length - failed.length}/${results.length} PASS，失败 ${failed.length}`);
  process.exit(failed.length ? 1 : 0);
}

main().catch((e) => { writeFileSync(join(OUT_DIR, `e2e-results-${MODE}.json`), JSON.stringify(results, null, 2)); console.error("E2E ERROR:", e); process.exit(2); });
