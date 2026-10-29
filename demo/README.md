# Go 服务端 SDK Demo

单进程 Web 聊天示例：浏览器通过 HTTP / WebSocket 访问 Demo，Demo 调用 `drim.Client`，并把签名回调转换成页面推送。
前端是原生 ES 模块，无构建步骤；Go 只使用标准库和本仓库 SDK。

本 Demo 需要 Go 1.23 或以上。它与 [Go 客户端 Demo](https://github.com/DeepRespond-AI/deeprespond-im-go-sdk/tree/main/demo) 不同：后者使用用户长连接，本 Demo 使用应用 SDK 加真实设备接口，并通过签名回调向页面推送。
两者都使用 `demo_sid` Cookie，同一主机同时体验时请使用不同浏览器或无痕上下文。

## 身份与权限

- **应用身份**：SDK 的 App Token 用于用户、好友、群组、消息发送等业务服务端操作。浏览器不能取得应用凭据。
- **设备身份**：密码登录产生真实设备会话，User Token 只保存在后端。`device.go` 负责用户历史、消息查询、撤回、原子改密和令牌续期。
- **授权边界**：每个浏览器动作先验证真实设备；群操作读取当前成员与群配置，不能依赖页面按钮或旧缓存授权。
- **历史可见性**：管理 SDK 的 `Conversations.Messages` 面向应用全量数据，不能替代用户历史。本示例使用客户端接口过滤逐用户删除、清空及入退群范围。
- **退出与吊销**：退出登录先吊销真实设备；旧回调通过有效会话列表核对，保留吊销之后的新登录。改密保留当前设备并轮换令牌。
- **附件**：仅 PNG/JPEG/GIF/WebP 按字节识别后内联，其余强制下载；忽略用户提供的响应类型，并设置 `nosniff`、沙箱和禁止缓存。

`device.go` 是 Demo 的设备适配层，不属于服务端 SDK 的公共 API。业务服务端通常为客户端 SDK 签发登录票据，见 `examples/loginticket`。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `main.go` | 参数解析与服务启动 |
| `http.go`、`media.go` | 路由、登录注册、上传与安全下载 |
| `bridge.go`、`wsbridge.go` | 浏览器桥接与 WebSocket 帧处理 |
| `state.go`、`views.go` | 共享状态、纯视图和快照复制 |
| `device.go`、`data.go` | 设备接口、SDK 列表、历史和权限 |
| `actions.go` | 验证设备、分发动作、复制回复快照 |
| `actions_chat.go`、`actions_contacts.go` | 聊天、好友与黑名单 |
| `actions_groups.go`、`actions_account.go` | 群权限、账号和设备管理 |
| `messages.go`、`message_events.go` | 发送与权威消息同步 |
| `callbacks.go`、`push.go`、`sessions.go` | 回调、页面推送、会话结束 |
| `web/app.js` | 页面入口和交互组合 |
| `web/bridge.js`、`web/state.js` | 通信与页面状态 |
| `web/auth.js`、`web/chat.js` | 登录和聊天、历史翻页 |
| `web/render.js`、`web/events.js`、`web/ui.js` | 列表面板、推送处理、通用界面工具 |
| `*_test.go`、`testdata/` | 单元与并发回归、真实服务和截图 E2E |

所有 Demo 源文件控制在 500 行以内，由结构测试约束。共享缓存由 `AppState.mu` 保护；回复复制为独立快照后发送，网络调用不持有状态锁。

## 下载与运行

[下载 v1.0.1 源码 ZIP](https://github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/archive/refs/tags/v1.0.1.zip)。

解压后进入仓库根目录，再执行下面的命令。

```bash
go run ./demo \
  --api http://127.0.0.1:18480 \
  --app-key '组织#应用' \
  --client-id "$IM_CLIENT_ID" --client-secret "$IM_CLIENT_SECRET" \
  --addr 127.0.0.1:8092 \
  --callback-credentials '地址ID:回调密钥'
```

启动后打开 `http://127.0.0.1:8092`，用该应用下的账号登录；双账号体验使用独立浏览器上下文。
从 仓库根目录 目录执行 `go build -o /tmp/im-go-server-demo ./demo` 可生成内嵌页面的二进制，无需安装 Node.js。

| 参数 | 默认值或说明 |
| --- | --- |
| `--api` | `http://127.0.0.1:18480` |
| `--app-key` | 可用环境变量 `APP_KEY` |
| `--client-id` / `--client-secret` | 可用 `IM_CLIENT_ID` / `IM_CLIENT_SECRET` |
| `--addr` | `127.0.0.1:8092` |
| `--callback-credentials` | `endpoint_id:secret`，多个以逗号分隔；缺少时没有实时推送 |
| `--debug` | 开启 SDK 调试日志 |

### 本机开发服务

以下需要完整 IM 仓库，不在 SDK 压缩包中。在仓库根目录准备基础设施，并启动独立的测试应用：

```bash
make infra-up
go build -o /tmp/im-harness ./sdk/conformance/harness
/tmp/im-harness -env .env -addr 127.0.0.1:18880 -control 127.0.0.1:18890 \
  -db im_go_demo_test -redis-prefix go-demo-test:
```

harness 的 `/env` 和 `/credentials` 提供本机测试配置。控制接口只用于开发环境。
通过 `POST /console/reauth` 重新验证后，向 `POST /console` 发送代理请求创建回调地址：

```json
{
  "method": "POST",
  "path": "/apps/<app_name>/callback/endpoints",
  "body": {
    "name": "go-server-demo",
    "url": "http://127.0.0.1:8092/im/callback",
    "event_types": ["message.sent", "message.recalled", "message.edited", "friend.added",
      "friend.removed", "friend.updated", "friend_request.received", "blacklist.changed",
      "group.created", "group.info_changed", "group.dismissed", "group.members_added",
      "group.members_removed", "group.members_updated", "user.profile_changed",
      "user.sessions_revoked", "user.relogin_required", "presence.changed"],
    "filters": {"message_include_tips": true},
    "batch_size": 10,
    "timeout_ms": 5000
  }
}
```

把响应的 `endpoint.endpoint_id` 和 `secret` 作为 Demo 的回调凭据启动。
再通过同一控制接口代理 `POST /apps/<app_name>/callback/endpoints/<endpoint_id>/verify`，确认 `ok=true`。

顺序是：创建未验证的回调地址 → 将地址 ID 和密钥传入 Demo 并启动 → 验证回调地址 → 登录和收发消息。
验证请求发送到 Demo 的 `/im/callback`，无需业务代码手写应答。
开发 harness 允许本机回调；其他部署环境需要使用 IM 服务能访问的回调地址，生产回调的 HTTPS 与地址规则见官网回调文档。

## 排查

| 现象 | 检查 |
| --- | --- |
| 缺少应用凭据 | 设置 `IM_CLIENT_ID`、`IM_CLIENT_SECRET` 或对应命令行参数 |
| 登录失败 | AppKey、账号密码、应用状态与密码登录策略是否匹配 |
| 回调验证失败 | IM 服务能否访问 `/im/callback`，地址 ID、密钥和反向代理路径是否正确 |
| 能发送但对方没有实时更新 | 回调地址是否验证通过，事件订阅是否完整，签名密钥是否一致 |
| 两套 Demo 互相覆盖登录 | 使用不同浏览器上下文；Cookie 不按端口隔离 |
| 重启后会话丢失 | 当前示例采用进程内状态，重启后重新登录 |

## 功能与接口

| 页面功能 | 实际接口 |
| --- | --- |
| 注册、好友、黑名单、群组 | SDK `Users` / `Friends` / `Blacklist` / `Groups` |
| 发送文本、引用、附件 | SDK `Messages.Send` / `Media.Upload` |
| 会话列表、已读、逐用户删除与清空 | SDK `Conversations` 的用户参数接口 |
| 最新历史、更早历史 | 设备 `/conversations/{id}/messages`，保留服务端游标和 `has_more` |
| 编辑、撤回、发送回调 | 设备 `/messages/lookup` 重读权威版本后推送 |
| 撤回 | 设备接口校验发送者、角色和撤回窗口 |
| 修改密码 | 设备 `/me/password` 原子校验旧密码并轮换令牌 |
| 设备列表、踢设备、注销 | SDK `Users.Sessions` / `Users.Kick` |
| 好友在线状态 | SDK `Presence` 查询和回调；真实在线状态取决于客户端长连接 |

输入中提示仅在本进程转发，多实例不可用。消息回应、会话置顶、好友申请和群邀请的接受/拒绝不由服务端 OpenAPI 提供，界面明确提示；直接添加好友、加群是应用服务端能力。

## 回归测试

```bash
# 仓库根目录；按本机已安装工具链设置 GOTOOLCHAIN
go test -race ./...
IM_TEST_HARNESS=http://127.0.0.1:18890 go test -tags integration ./integration/...

# 启动 Demo 并验证回调之后；Python 安装 httpx、websockets
IM_DEMO_TEST_URL=http://127.0.0.1:8092 IM_TEST_HARNESS=http://127.0.0.1:18890 \
  python demo/testdata/security_e2e.py

# 安装 Playwright 后运行浏览器测试；也可指定本机已有的包和 Chromium 路径
export IM_DEMO_TEST_URL=http://127.0.0.1:8092
export IM_TEST_HARNESS=http://127.0.0.1:18890
node demo/testdata/e2e.mjs desktop
node demo/testdata/e2e.mjs narrow
```

浏览器脚本支持 `PLAYWRIGHT_MODULE`、`PLAYWRIGHT_CHROMIUM_EXECUTABLE`、`E2E_OUTPUT_DIR`，默认产物目录是 `artifacts/server-go-demo-e2e`。
安全回归会吊销整个测试应用的旧会话，必须使用独立的 harness 应用，且与浏览器 E2E 分开运行。
