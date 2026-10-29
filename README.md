# DeepRespond IM 服务端 Go SDK

版本 **v1.0.1**，MIT 许可证，Go **1.23 或以上**，仅依赖标准库。

使用应用身份调用 OpenAPI，管理用户、群组、消息、文件、聊天室、推送与音视频频道，并签发用户登录凭证。提供签名回调的校验、防重放、事件去重与处理器。
模块 `github.com/DeepRespond-AI/deeprespond-im-server-go-sdk`，包名 `drim`；回调在 `callback`，模拟服务与测试辅助在 `drimtest`。

## 安装

```bash
go get github.com/DeepRespond-AI/deeprespond-im-server-go-sdk@v1.0.1
```

```go
import drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
import "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
```

[在线文档](https://www.syznlink.com/docs/server-sdk/go/) · [Demo 指南](demo/README.md) · [更新记录](CHANGELOG.md)

客户端应用需要以 IM 用户身份登录、建立长连接和收发消息时，请使用 [Go 客户端 SDK](https://github.com/DeepRespond-AI/deeprespond-im-go-sdk)。客户端和服务端 SDK 使用不同身份与凭证。

## 调用 OpenAPI

```go
import drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"

client, err := drim.New(drim.Config{
    BaseURL:      "https://im.example.com",
    OrgName:      "1100250925",
    AppName:      "demo",
    ClientID:     os.Getenv("IM_CLIENT_ID"),
    ClientSecret: os.Getenv("IM_CLIENT_SECRET"),
})
if err != nil {
    return err
}
defer client.Close()

// 用户登录业务系统后，为他签发登录凭证，交给 App 用客户端 SDK 登录
ticket, err := client.Users.IssueLoginTicket(ctx, "zhangsan", drim.IssueLoginTicketParams{AutoCreate: true})

// 以业务的通知账号发单聊；业务单号作为 client_msg_id，重试也不会重复发送
msg, err := client.Messages.Send(ctx, drim.SendMessageParams{
    From: "notice", ToUser: "zhangsan", ClientMsgID: "order-1001-paid",
    Type: "custom", Body: drim.JSON(`{"card":"order","order_id":"1001"}`),
})

for user, err := range client.Users.All(ctx, drim.ListUsersParams{Status: "active"}) { // 自动翻页
    if err != nil { return err }
    _ = user
}
```

- **客户端在进程中复用**：它可以被多个协程同时使用，持有连接池和 App Token；App Token 由 SDK 换取、缓存、提前续期，失效后自动重新换取。
- **`ctx` 是整次调用的总时限**（含重试、限流等待和等待换取令牌）；每次尝试的时限是 `Config.Timeout`（默认 30 秒）。单次调用的选项通过 `ctx` 传入：`drim.WithRequestID`、`WithAttemptTimeout`、`WithMaxAttempts`、`WithMaxRetryAfter`。
- **重试**：查询、带幂等键的写操作（发消息）、本身幂等的写操作在网络错误、超时、5xx 时自动重试；建群、设置密码等不幂等的写操作发出后不重试。被限流时按 `Retry-After` 等待后重发：应用额度用完时整个客户端一起等待，某个群、聊天室被限流时只有这个请求等待。
- **三种状态的参数**：`drim.Field[T]` 区分“不传”（零值）、`drim.Null[T]()` 和 `drim.Set(v)`，如修改资料时 `Attributes: drim.Set(map[string]drim.Field[string]{"level": drim.Null[string]()})` 删除一项属性；只有“不传”和“传值”的用指针（`drim.String`、`drim.Int64`）。
- **原样发送 JSON**：消息的 `Body`、`Ext` 是 `json.RawMessage`（`drim.JSON(s)`，或 `drim.Marshal(v)` 编码时不转义 `<>&`），发送时只去掉空白，写法逐字节保留；响应中的 `Body` 与服务端返回的逐字节相同。每个响应类型都有 `Raw`。

### 错误

```go
var e *drim.Error
if errors.As(err, &e) {
    log.Print(e.Code, e.Reason(), e.RequestID) // 按 Code 判断，不解析 Message
}
drim.IsCode(err, drim.CodeAlreadyExists)
drim.IsTemporary(err)          // 网络错误、超时、5xx、rate_limited
```

自动重试后得到 `already_exists`、`not_found`、`version_conflict` 时，`e.Retried` 为真：前一次尝试可能已经执行。批量接口逐项的失败在结果中（`FailedItems()`、`OK()`、`Err()`）。

### 文件

```go
f, _ := os.Open("a.jpg")
st, _ := f.Stat()
file, err := client.Media.Upload(ctx, f, st.Size(), drim.UploadParams{Purpose: "attachment", Kind: "image"})
client.Messages.Send(ctx, drim.SendMessageParams{From: "notice", ToUser: "zhangsan", Type: "image",
    Body: drim.JSON(`{"url":"` + file.URL + `"}`)})
n, err := client.Media.Download(ctx, file.URL, w)
```

不超过 32 MB 的一次上传，更大的分片并发上传；中断后可以用 `ResumeUpload(ctx, fileID, r, size, params)` 续传。

## 接收回调

```go
import "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"

h, err := callback.NewHandler(callback.Config{
    Secrets: callback.StaticSecrets{"1840200000000000007": {"cbs_新密钥…", "cbs_旧密钥…"}},
    Nonces:  callback.NewMemoryNonceStore(),        // 多实例部署时换成共享存储
    Dedup:   callback.NewMemoryDedupStore(24 * time.Hour),
    Events: callback.EventHandlers{
        MessageSent: func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
            return archive(d.MessageID, d.Body) // d.Body 与请求体中的逐字节相同
        },
    },
    Hooks: callback.HookHandlers{
        MessageBeforeSend: func(ctx context.Context, req *callback.MessageBeforeSend) (callback.Verdict, error) {
            if containsBannedWord(req.Body) {
                return callback.Reject("forbidden_word", "消息中有不允许的内容"), nil
            }
            return callback.Allow(), nil
        },
    },
})
http.Handle("POST /im/callback", h) // Gin：gin.WrapH(h)；Echo：echo.WrapHandler(h)
```

- 重复的事件（同一个事件 ID）不再调用处理函数，返回 2xx；一批事件中有处理失败的，停止处理并整批返回 503，服务端稍后重发（已完成的被跳过）。处理函数返回 `callback.RetryAfter(d, err)` 时带上 `Retry-After`。
- 同步回调的处理函数的 `ctx` 截止时间为 `timeout_ms` 减 50 毫秒，超时或出错时返回 `HookFallback`（没有设置时 503）；返回的结果在本地检查，不合规的不发出。
- 量大的消息抄送用异步方式：`Queue` 交给调用方的持久队列，入队即完成。

### 共享存储（Redis）

SDK 只依赖标准库，没有内置 Redis 的实现。用 `github.com/redis/go-redis/v9` 实现三个接口的写法：

```go
type RedisStores struct{ R *redis.Client; Prefix string }

// 令牌存储（drim.TokenStore）：值为 "{到期的毫秒}:{令牌}"
func (s RedisStores) Get(ctx context.Context, key string) (string, time.Time, bool, error) {
    v, err := s.R.Get(ctx, s.Prefix+key).Result()
    if errors.Is(err, redis.Nil) { return "", time.Time{}, false, nil }
    if err != nil { return "", time.Time{}, false, err }
    ms, token, ok := strings.Cut(v, ":")
    n, _ := strconv.ParseInt(ms, 10, 64)
    return token, time.UnixMilli(n), ok, nil
}
func (s RedisStores) Set(ctx context.Context, key, token string, expiresAt time.Time) error {
    return s.R.Set(ctx, s.Prefix+key, fmt.Sprintf("%d:%s", expiresAt.UnixMilli(), token), time.Until(expiresAt)).Err()
}
var compareAndDelete = redis.NewScript(`local v = redis.call('GET', KEYS[1])
if v then local i = string.find(v, ':', 1, true)
  if i and string.sub(v, i + 1) == ARGV[1] then return redis.call('DEL', KEYS[1]) end end
return 0`)
func (s RedisStores) CompareAndDelete(ctx context.Context, key, token string) error {
    return compareAndDelete.Run(ctx, s.R, []string{s.Prefix + key}, token).Err()
}

// 随机数（callback.NonceStore）
func (s RedisStores) Seen(ctx context.Context, nonce string, ttl time.Duration) (bool, error) {
    ok, err := s.R.SetNX(ctx, s.Prefix+"nonce:"+nonce, 1, ttl).Result()
    return !ok, err
}

// 事件去重（callback.DedupStore）：占用、完成、释放都按占用的令牌比较；完成后保留 7 天
var claim = redis.NewScript(`local v = redis.call('GET', KEYS[1])
if not v then redis.call('SET', KEYS[1], 'busy:' .. ARGV[1], 'PX', ARGV[2]) return 'acquired' end
if v == 'done' then return 'done' end
return 'busy'`)
var complete = redis.NewScript(`if redis.call('GET', KEYS[1]) == 'busy:' .. ARGV[1] then
  redis.call('SET', KEYS[1], 'done', 'PX', ARGV[2]) return 1 end
return 0`)
var release = redis.NewScript(`if redis.call('GET', KEYS[1]) == 'busy:' .. ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)
func (s RedisStores) Claim(ctx context.Context, id string, ttl time.Duration) (callback.ClaimState, string, error) {
    token := uuid.NewString()
    r, err := claim.Run(ctx, s.R, []string{s.Prefix + "event:" + id}, token, ttl.Milliseconds()).Text()
    switch {
    case err != nil: return 0, "", err
    case r == "acquired": return callback.Acquired, token, nil
    case r == "done": return callback.Done, "", nil
    }
    return callback.Busy, "", nil
}
func (s RedisStores) Complete(ctx context.Context, id, token string) error {
    return complete.Run(ctx, s.R, []string{s.Prefix + "event:" + id}, token, (7 * 24 * time.Hour).Milliseconds()).Err()
}
func (s RedisStores) Release(ctx context.Context, id, token string) error {
    return release.Run(ctx, s.R, []string{s.Prefix + "event:" + id}, token).Err()
}
```

## 测试自己的代码

```go
srv := drimtest.NewServer()            // 假的 OpenAPI
defer srv.Close()
srv.Fail("messages.send", "rate_limited", drimtest.FailOptions{RetryAfter: "1"})
client, _ := drim.New(srv.Config())
// ...
calls := srv.Calls("messages.send")    // 收到的请求

req := drimtest.SignedRequest(endpointID, drimtest.EventRequest(drimtest.Event{Type: "user.created", Data: data}), secret)
handler.ServeHTTP(recorder, req)        // 带正确签名的回调请求
```

`drimtest.SignatureVectors()` 是签名的测试向量（与服务端和对外文档相同），可以用来确认部署环境（如反向代理没有改动请求体）。

## Demo

`demo/` 是单进程 Web 即时通讯演示，使用本 SDK 完成业务服务端操作，通过签名回调实时推送。
设备适配层只在后端保存 User Token，处理用户可见历史、撤回、原子改密和登录有效性；它不扩展 SDK 的公共 API。
文件结构、启动方式和带截图的回归命令见 [demo/README.md](demo/README.md)。

服务端 SDK 使用应用权限，调用方必须在业务接口中校验当前用户、会话归属和群角色。
尤其 `Conversations.Messages` 是管理视角，不过滤逐用户删除、清空及入退群范围；用户界面应通过客户端接口读取历史。
`Media.Download` 只写出文件字节；HTTP 代理需要自行设置安全的响应类型、下载方式和缓存策略，不能信任用户传入的 MIME。

## 开发

- 类型、参数和方法由 `spec` 生成（`*_gen.go`）：改了接口描述后运行 `go generate`（即 `python3 internal/codegen/generate.py`，需要 PyYAML）。
- `make sdk-server-go-test`：生成代码一致性、gofmt、go vet、`go test -race`。
- `make sdk-server-go-integration`：连接 `sdk/conformance/harness` 的集成测试和回调的端到端测试（`go test -tags integration ./integration/`）。


独立 RTC 频道管理已提供生成的强类型 `Channels/channels` API：创建、修改、关闭、票据、成员管理、统计与更正列表。Go 使用 `RTCChannel*Params/Result`，Node.js/Python 返回具体模型；Python 支持同步/异步。期限保留未设置/null/值三态，票据签发不自动重试。旧 Go 原始 JSON 接口需迁移到新参数/结果；管理 API 不能代替用户登录获取媒体 Token。完整契约及计费规则见仓库 `docs/RTC频道/4.实现与部署.md`。
