// 示例：以业务的通知账号发单聊和群聊消息；业务单号作为 client_msg_id，业务层重试也不会重复发送。
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

func main() {
	client, err := drim.New(drim.Config{
		BaseURL:      os.Getenv("IM_BASE_URL"),
		OrgName:      os.Getenv("IM_ORG_NAME"),
		AppName:      os.Getenv("IM_APP_NAME"),
		ClientID:     os.Getenv("IM_CLIENT_ID"),
		ClientSecret: os.Getenv("IM_CLIENT_SECRET"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// 整次调用的总时限（含重试和限流等待）由 ctx 给出
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = drim.WithRequestID(ctx, "order-svc.1001") // 服务端的日志中能按它查到这次调用

	body, _ := drim.Marshal(map[string]any{"card": "order", "order_id": "1001", "title": "订单已支付 <¥99.00>"})
	result, err := client.Messages.Send(ctx, drim.SendMessageParams{
		From: "notice", ToUser: "zhangsan", ClientMsgID: "order-1001-paid", Type: "custom", Body: body,
		Push: &drim.MessagePushOptions{Title: "订单通知", Body: "您的订单已支付"},
	})
	var e *drim.Error
	switch {
	case errors.As(err, &e) && e.Code == drim.CodeMessageRejected:
		log.Printf("内容被拒绝（%s）", e.Reason())
	case err != nil:
		log.Fatal(err)
	case result.Duplicate:
		log.Printf("重复提交，返回第一次的消息 %s", result.MessageID)
	default:
		log.Printf("已发送：会话 %s 序号 %d", result.ConversationID, result.Seq)
	}

	// 群聊：From 为空时以系统身份发送
	if _, err := client.Messages.Send(ctx, drim.SendMessageParams{ToGroup: "1840012345678901", Type: "text", Body: drim.JSON(`{"text":"今晚 8 点维护"}`)}); err != nil {
		log.Print(err)
	}
}
