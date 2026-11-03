// Package drim 是深应智能（DeepRespond）IM 的服务端 Go SDK：在业务服务端调用 OpenAPI（管理用户、好友、群组、消息、文件、
// 推送设置、聊天室、内容安全、通话和在线状态，签发登录凭证）。接收回调见子包 callback，测试辅助见子包 drimtest。
//
// 一个客户端对应一个应用，可以被多个协程同时使用，应在进程中复用：
//
//	client, err := drim.New(drim.Config{
//		BaseURL:      "https://im.example.com",
//		OrgName:      "1100250925",
//		AppName:      "demo",
//		ClientID:     os.Getenv("IM_CLIENT_ID"),
//		ClientSecret: os.Getenv("IM_CLIENT_SECRET"),
//	})
//	ticket, err := client.Users.IssueLoginTicket(ctx, "zhangsan", drim.IssueLoginTicketParams{AutoCreate: true})
//
// 每个方法的第一个参数是 context.Context：截止时间是整次调用的总时限（含重试、限流等待和等待换取令牌）。
// 返回的错误都是 *Error（errors.As），按 Code 和 Details 判断。
//
// 行为规则见仓库中的 docs/server-go。类型、参数和方法由 sdk/server/spec 生成（*_gen.go，go generate 重新生成）。
package drim

//go:generate python3 internal/codegen/generate.py
