// 示例：回调接收端。校验签名、防重放、按事件 ID 去重由 SDK 完成，业务只写处理函数（《server-go/2.接口设计》第 9 节）。
// 多个实例部署时，随机数和去重换成共享存储（如 Redis，见 README）。
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
)

func main() {
	fallback := callback.Allow() // 处理函数出错或超时时放行（服务端看来是成功的响应）
	handler, err := callback.NewHandler(callback.Config{
		// 地址 ID → 密钥（控制台新建地址时显示；轮换期间新旧两个都配置）
		Secrets: callback.StaticSecrets{os.Getenv("IM_CALLBACK_ENDPOINT_ID"): strings.Split(os.Getenv("IM_CALLBACK_SECRETS"), ",")},
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(24 * time.Hour),
		Events: callback.EventHandlers{
			MessageSent: func(ctx context.Context, ev *callback.Event, d *callback.MessageSent) error {
				if d.Truncated || !d.ContentAvailable {
					return nil // 内容被截短或已不可用：需要时按 message_id 调用 OpenAPI 读取
				}
				// d.Body 与请求体中的逐字节相同，可以直接存档或验签
				log.Printf("消息 %s 序号 %d：%s", d.MessageID, d.Seq, d.Body)
				return nil
			},
			UserCreated: func(ctx context.Context, ev *callback.Event, d *callback.UserCreated) error {
				log.Printf("新用户 %s（%s）", d.Username, d.CreatedVia)
				return nil
			},
			Unknown: func(ctx context.Context, ev *callback.Event) error {
				log.Printf("SDK 还不认识的事件 %s", ev.Type)
				return nil
			},
		},
		Hooks: callback.HookHandlers{
			MessageBeforeSend: func(ctx context.Context, req *callback.MessageBeforeSend) (callback.Verdict, error) {
				var body struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(req.Body, &body)
				if strings.Contains(body.Text, "加微信") {
					return callback.Reject("forbidden_word", "消息中有不允许的内容"), nil
				}
				return callback.Allow(), nil
			},
		},
		HookFallback: &fallback,
		Logger:       slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /im/callback", handler)
	log.Fatal(http.ListenAndServe(":8090", mux))
}
