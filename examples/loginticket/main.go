// 示例：业务服务端为已登录的用户签发 IM 登录凭证，交给 App 用客户端 SDK 登录（《server-go/1.功能概述文档》第 9 节）。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

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

	// App 在业务系统登录后调用这个接口取 IM 的登录凭证；凭证无效时 App 会再取一次，这里要允许重试
	http.HandleFunc("POST /api/im-ticket", func(w http.ResponseWriter, r *http.Request) {
		userID := currentUser(r) // 业务系统的用户 ID 直接作为 IM 的用户名
		ticket, err := client.Users.IssueLoginTicket(r.Context(), userID, drim.IssueLoginTicketParams{AutoCreate: true})
		if drim.IsCode(err, drim.CodeUserDisabled) {
			http.Error(w, "账号已被封禁", http.StatusForbidden)
			return
		}
		if err != nil {
			log.Printf("签发 IM 登录凭证失败：%v", err) // 错误中不含凭证和令牌
			http.Error(w, "暂时不可用", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ticket": ticket.Ticket, "expires_in": ticket.ExpiresIn})
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func currentUser(r *http.Request) string { return r.Header.Get("X-User-ID") } // 由业务系统的登录态得出
