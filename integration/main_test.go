//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

// harness 是一致性测试的环境准备程序（sdk/conformance/harness）的控制接口，可用 IM_TEST_HARNESS 修改。
var harness = func() string {
	if v := os.Getenv("IM_TEST_HARNESS"); v != "" {
		return v
	}
	return "http://127.0.0.1:18490"
}()

var env struct {
	APIURL       string `json:"api_url"`
	OrgName      string `json:"org_name"`
	AppName      string `json:"app_name"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	GameRegion   string `json:"game_region"` // harness 打开游戏房间时登记的大区
}

var envErr error

// direct 不经过代理（本机的环境中有 http_proxy）。
var direct = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}

func TestMain(m *testing.M) {
	envErr = load()
	os.Exit(m.Run())
}

func load() error {
	for _, path := range []string{"/env", "/credentials"} {
		resp, err := direct.Get(harness + path)
		if err != nil {
			return err
		}
		err = json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func requireEnv(t *testing.T) {
	t.Helper()
	if envErr != nil {
		t.Skipf("连不上一致性测试的服务端 %s：%v", harness, envErr)
	}
}

func newClient(t *testing.T) *drim.Client {
	t.Helper()
	current = t
	requireEnv(t)
	client, err := drim.New(drim.Config{
		BaseURL:      env.APIURL,
		OrgName:      env.OrgName,
		AppName:      env.AppName,
		ClientID:     env.ClientID,
		ClientSecret: env.ClientSecret,
		HTTPClient:   &http.Client{Transport: &http.Transport{Proxy: nil}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func uniq(prefix string) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// control 调用 harness 的控制接口（如以租户管理员身份调用控制台接口）。
func control(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, harness+path, reader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := direct.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// current 是正在运行的测试（集成测试不并行）：must 出错时让它失败。
var current *testing.T

func must[T any](v T, err error) T {
	if err != nil {
		current.Helper()
		current.Fatal(err)
	}
	return v
}

func check(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

func code(err error) string {
	var e *drim.Error
	if err == nil {
		return ""
	}
	if errors.As(err, &e) {
		return e.Code
	}
	return fmt.Sprintf("%T", err)
}
