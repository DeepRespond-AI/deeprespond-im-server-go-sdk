package main

// 令牌轮换与密码更新走同一个设备，不额外创建登录。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentRequestsRefreshTokenOnce(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/client/v1/sessions/refresh" {
			if refreshes.Add(1) != 1 {
				t.Error("发生了重复续期")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"code": "unauthenticated"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "alice"})
	}))
	defer server.Close()
	device := newDevice(server.URL, map[string]any{"access_token": "expired", "refresh_token": "initial"})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := device.request("GET", "/client/v1/me", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatal("未正确轮换令牌")
	}
}

func TestPasswordChangeRotatesTokensWithoutNewDevice(t *testing.T) {
	var passwordUpdates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "PUT" && r.URL.Path == "/client/v1/me/password" {
			passwordUpdates.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["current_password"] != "old-password" || body["new_password"] != "new-password" {
				t.Error(body)
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "after", "refresh_token": "after-refresh"})
			return
		}
		if r.URL.Path != "/client/v1/me" {
			t.Errorf("意外的登录请求: %s", r.URL.Path)
		}
		expected := "Bearer before"
		if passwordUpdates.Load() > 0 {
			expected = "Bearer after"
		}
		if r.Header.Get("Authorization") != expected {
			t.Error("没有采用轮换后的令牌")
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "alice"})
	}))
	defer server.Close()
	f := setupDemo(t)
	f.session.device = newDevice(server.URL, map[string]any{"access_token": "before"})
	_, err := f.st.dispatch(f.session, "set_password", map[string]any{"old": "old-password", "new": "new-password"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.dispatch(f.session, "friends", nil); err != nil {
		t.Fatal(err)
	}
	if f.session.IMSessionID != "real-session" || len(f.sdk.Calls("users.set_password")) != 0 || passwordUpdates.Load() != 1 {
		t.Fatal("改密改变了设备或使用了管理改密")
	}
}
