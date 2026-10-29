package main

// 设备身份只保存在后端；用户消息读取与本人操作不能借用管理员权限。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

type deviceClient struct {
	mu           sync.Mutex
	apiURL       string
	accessToken  string
	refreshToken string
}

func newDevice(apiURL string, auth map[string]any) *deviceClient {
	return &deviceClient{apiURL: strings.TrimRight(apiURL, "/"), accessToken: strOf(auth["access_token"]), refreshToken: strOf(auth["refresh_token"])}
}

func deviceRequest(apiURL, method, path, token string, body any) (map[string]any, error) {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, apiURL+path, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := direct.Do(req)
	if err != nil {
		return nil, demoErr("network_error", "客户端接口暂时不可达")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(raw) > 2<<20 || (len(raw) > 0 && json.Unmarshal(raw, &out) != nil) || out == nil {
		return nil, demoErr("invalid_response", "客户端接口响应格式非法")
	}
	if resp.StatusCode >= 400 {
		details := out
		if nested, ok := out["error"].(map[string]any); ok {
			details = nested
		}
		code, message := strOf(details["code"]), strOf(details["message"])
		if code == "" {
			code = "client_error"
		}
		if message == "" {
			message = fmt.Sprintf("客户端请求失败（HTTP %d）", resp.StatusCode)
		}
		return nil, &drim.Error{StatusCode: resp.StatusCode, Code: code, Message: message}
	}
	return out, nil
}

func (d *deviceClient) request(method, path string, body any) (map[string]any, error) {
	// 续期会轮换 refresh token；串行请求避免并发刷新触发重放检测。
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.accessToken == "" {
		return nil, &drim.Error{StatusCode: 401, Code: "unauthenticated", Message: "请重新登录"}
	}
	out, err := deviceRequest(d.apiURL, method, path, d.accessToken, body)
	if e, ok := err.(*drim.Error); ok && e.StatusCode == 401 && d.refreshToken != "" {
		auth, refreshErr := deviceRequest(d.apiURL, "POST", "/client/v1/sessions/refresh", "", map[string]any{"refresh_token": d.refreshToken})
		if refreshErr != nil {
			return nil, refreshErr
		}
		if err = d.setTokens(auth); err != nil {
			return nil, err
		}
		out, err = deviceRequest(d.apiURL, method, path, d.accessToken, body)
	}
	if err == nil && method == "PUT" && path == "/client/v1/me/password" {
		err = d.setTokens(out)
	}
	return out, err
}

func (d *deviceClient) setTokens(auth map[string]any) error {
	d.accessToken = strOf(auth["access_token"])
	d.refreshToken = strOf(auth["refresh_token"])
	if d.accessToken == "" {
		return demoErr("invalid_response", "响应缺少 access_token")
	}
	return nil
}

func clientLogin(orgName, appName, apiURL, username, password string) (map[string]any, error) {
	return deviceRequest(strings.TrimRight(apiURL, "/"), "POST", "/client/v1/sessions", "", map[string]any{
		"app_key": orgName + "#" + appName, "grant_type": "password", "username": username, "password": password,
		"device_id": "go-demo-" + randHex(8), "device_name": "Go Server Demo", "platform": "web", "sdk_version": "go-server-demo",
	})
}

func (st *AppState) deviceOf(s *BrowserSession) (*deviceClient, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s == nil || s.dead || s.closing || s.device == nil {
		return nil, &drim.Error{StatusCode: 401, Code: "unauthenticated", Message: "会话已失效，请重新登录"}
	}
	return s.device, nil
}

func (st *AppState) userDevice(username string) (*deviceClient, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ctx := st.contexts[username]; ctx != nil {
		for _, s := range ctx.Sessions {
			if !s.dead && !s.closing && s.device != nil {
				return s.device, nil
			}
		}
	}
	return nil, &drim.Error{StatusCode: 401, Code: "unauthenticated", Message: "用户没有有效的设备会话"}
}

func conversationPath(id string) string { return "/client/v1/conversations/" + url.PathEscape(id) }

func (st *AppState) authenticate(s *BrowserSession) (*deviceClient, error) {
	d, err := st.deviceOf(s)
	if err != nil {
		return nil, err
	}
	_, err = d.request("GET", "/client/v1/me", nil)
	return d, err
}
