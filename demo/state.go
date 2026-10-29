package main

// 应用状态：所有共享缓存和浏览器会话由 mu 保护。

import (
	"strings"
	"sync"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

// BrowserSession 一次浏览器登录，对应一个真实设备和当前桥接连接。
type BrowserSession struct {
	SID         string
	Username    string
	IMSessionID string
	ws          *bridgeConn
	dead        bool
	device      *deviceClient
	closing     bool
}

// UserContext 一个登录用户在后端的状态：各列表的缓存和消息缓存，由回调与动作结果增量维护。
type UserContext struct {
	Username  string
	Me        map[string]any
	Sessions  []*BrowserSession
	Friends   map[string]map[string]any
	Groups    map[string]map[string]any
	Convs     map[string]map[string]any
	Msgs      map[string][]map[string]any
	Resyncing bool
}

// AppState 全部共享状态；mu 保护以下所有字段。
type AppState struct {
	mu sync.Mutex

	client *drim.Client
	apiURL string

	orgName string
	appName string
	// callbackSecrets 回调凭据：回调地址 ID → 密钥列表（轮换期间新旧两个）
	callbackSecrets map[string][]string

	contexts map[string]*UserContext
	sessions map[string]*BrowserSession
}

func newState(client *drim.Client, apiURL, appKey string, callbackSecrets map[string][]string) *AppState {
	orgName, appName, _ := strings.Cut(appKey, "#")
	if callbackSecrets == nil {
		callbackSecrets = map[string][]string{}
	}
	return &AppState{
		client:          client,
		apiURL:          strings.TrimRight(apiURL, "/"),
		orgName:         orgName,
		appName:         appName,
		callbackSecrets: callbackSecrets,
		contexts:        map[string]*UserContext{},
		sessions:        map[string]*BrowserSession{},
	}
}

// contextOf 取（或建）用户的上下文；持锁调用。
func (st *AppState) contextOf(username string) *UserContext {
	ctx := st.contexts[username]
	if ctx == nil {
		ctx = &UserContext{
			Username: username,
			Me:       map[string]any{"username": username},
			Friends:  map[string]map[string]any{},
			Groups:   map[string]map[string]any{},
			Convs:    map[string]map[string]any{},
			Msgs:     map[string][]map[string]any{},
		}
		st.contexts[username] = ctx
	}
	return ctx
}
