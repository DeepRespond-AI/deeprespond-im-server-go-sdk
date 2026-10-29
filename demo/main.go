package main

// 命令行入口：创建 SDK、回调处理器和 HTTP 服务。

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

//go:embed web
var webFS embed.FS

var (
	flagAddr      = flag.String("addr", "127.0.0.1:8092", "HTTP 监听地址")
	flagAPI       = flag.String("api", "http://127.0.0.1:18480", "IM 服务端 REST 地址")
	flagAppKey    = flag.String("app-key", os.Getenv("APP_KEY"), "应用 AppKey（组织#应用）")
	flagClientID  = flag.String("client-id", os.Getenv("IM_CLIENT_ID"), "应用服务端凭据 client_id")
	flagSecret    = flag.String("client-secret", os.Getenv("IM_CLIENT_SECRET"), "应用服务端凭据 client_secret")
	flagCallbacks = flag.String("callback-credentials", "", "回调凭据 endpoint_id:secret，多个用逗号分隔")
	flagDebug     = flag.Bool("debug", false, "打印 SDK 调试日志")
)

func parseCallbackCredentials(spec string) map[string][]string {
	out := map[string][]string{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, secret, _ := strings.Cut(item, ":")
		if id == "" || secret == "" {
			continue
		}
		out[id] = append(out[id], secret)
	}
	return out
}

func main() {
	flag.Parse()
	if *flagAppKey == "" || !strings.Contains(*flagAppKey, "#") {
		fmt.Fprintln(os.Stderr, "缺少 --app-key（或环境变量 APP_KEY），形如 6526900072#myapp")
		os.Exit(1)
	}
	if *flagClientID == "" || *flagSecret == "" {
		fmt.Fprintln(os.Stderr, "缺少 --client-id / --client-secret（应用的服务端凭据）")
		os.Exit(1)
	}
	orgName, appName, _ := strings.Cut(*flagAppKey, "#")
	cfg := drim.Config{
		BaseURL:      *flagAPI,
		OrgName:      orgName,
		AppName:      appName,
		ClientID:     *flagClientID,
		ClientSecret: *flagSecret,
	}
	if *flagDebug {
		cfg.Logger = slog.Default()
	}
	client, err := drim.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	st := newState(client, *flagAPI, *flagAppKey, parseCallbackCredentials(*flagCallbacks))
	handler, err := st.handler()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Go 服务端 SDK demo 已启动: http://%s  (api=%s app_key=%s 回调地址 %d 个)",
		*flagAddr, *flagAPI, *flagAppKey, len(st.callbackSecrets))
	server := &http.Server{Addr: *flagAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
