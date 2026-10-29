// 示例：从旧系统迁移用户。批量创建自动拆分（每批 100 个），本地限速把额度留给线上请求。
package main

import (
	"context"
	"fmt"
	"log"
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
		RateLimit:    &drim.RateLimit{PerSecond: 200}, // 按项数计
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	var users []drim.CreateUserParams
	for i := 0; i < 1000; i++ {
		users = append(users, drim.CreateUserParams{Username: fmt.Sprintf("legacy-%04d", i), Nickname: fmt.Sprintf("用户 %d", i)})
	}
	result, err := client.Users.BatchCreateAll(context.Background(), users)
	if result != nil {
		log.Printf("创建了 %d 个", len(result.Created))
		for _, failed := range result.FailedItems() {
			if failed.Code != drim.CodeAlreadyExists { // 已存在的视为上次已导入
				log.Printf("%s 失败：%s", failed.Key, failed.Code)
			}
		}
	}
	if err != nil {
		log.Fatalf("某一批整体失败，之后的没有提交：%v", err)
	}

	// 遍历全部用户（自动翻页）
	count := 0
	for _, err := range client.Users.All(context.Background(), drim.ListUsersParams{UsernamePrefix: "legacy-"}) {
		if err != nil {
			log.Fatal(err)
		}
		count++
	}
	log.Printf("共 %d 个", count)
}
