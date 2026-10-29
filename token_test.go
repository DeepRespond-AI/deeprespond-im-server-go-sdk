package drim_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

// memoryStore 是测试用的共享令牌存储。
type memoryStore struct {
	mu      sync.Mutex
	tokens  map[string]string
	expires map[string]time.Time
	getErr  error
	deletes int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{tokens: map[string]string{}, expires: map[string]time.Time{}}
}

func (m *memoryStore) Get(_ context.Context, key string) (string, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return "", time.Time{}, false, m.getErr
	}
	token, ok := m.tokens[key]
	return token, m.expires[key], ok, nil
}

func (m *memoryStore) Set(_ context.Context, key, token string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[key] = token
	m.expires[key] = expiresAt
	return nil
}

func (m *memoryStore) CompareAndDelete(_ context.Context, key, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens[key] == token {
		delete(m.tokens, key)
		m.deletes++
	}
	return nil
}

func (m *memoryStore) only() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.tokens {
		return k, v
	}
	return "", ""
}

func TestSharedTokenStore(t *testing.T) { // S-41
	srv := newServer(t)
	store := newMemoryStore()
	a := newClient(t, srv, func(c *drim.Config) { c.TokenStore = store })
	b := newClient(t, srv, func(c *drim.Config) { c.TokenStore = store })
	if _, err := a.Users.Get(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Users.Get(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Calls("app.token")); n != 1 {
		t.Fatalf("B 应当用存储中的令牌：%d", n)
	}
	key, token := store.only()
	if !strings.HasPrefix(key, "drim:token:") || len(key) != len("drim:token:")+16 || token != "fake-token-1" {
		t.Fatal(key, token)
	}
	srv.RevokeTokens()
	if _, err := b.Users.Get(context.Background(), "x"); err != nil { // B 换到新令牌并写入
		t.Fatal(err)
	}
	if _, token := store.only(); token != "fake-token-2" {
		t.Fatal(token)
	}
	if _, err := a.Users.Get(context.Background(), "x"); err != nil { // A 的旧令牌失效：不删除新令牌，改用它
		t.Fatal(err)
	}
	if _, token := store.only(); token != "fake-token-2" {
		t.Fatalf("A 不应删除 B 的新令牌：%q", token)
	}
	if n := len(srv.Calls("app.token")); n != 2 {
		t.Fatalf("A 应当用存储中的新令牌而不是自己换取：%d", n)
	}
	calls := srv.Calls("users.get")
	if last := calls[len(calls)-1].Header.Get("Authorization"); last != "Bearer fake-token-2" {
		t.Fatal(last)
	}
}

func TestTokenStoreErrorFallsBack(t *testing.T) { // S-43
	srv := newServer(t)
	store := newMemoryStore()
	store.getErr = errors.New("redis: connection refused")
	logs := &logBuffer{}
	client := newClient(t, srv, func(c *drim.Config) { c.TokenStore = store; c.Logger = logs.logger() })
	if _, err := client.Users.Get(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "退回本进程换取") {
		t.Fatal(logs.String())
	}
	if strings.Contains(logs.String(), srv.ClientSecret) || strings.Contains(logs.String(), "fake-token") {
		t.Fatal("日志中不能有密钥和令牌")
	}
}

func TestTokenMethod(t *testing.T) {
	srv := newServer(t)
	client := newClient(t, srv)
	token, err := client.Token(context.Background())
	if err != nil || token != "fake-token-1" {
		t.Fatal(token, err)
	}
	if body := srv.Calls("app.token")[0].JSON(); body["grant_type"] != "client_credentials" || body["client_id"] != srv.ClientID {
		t.Fatal(body)
	}
	if h := srv.Calls("app.token")[0].Header; h.Get("Authorization") != "" || h.Get("Content-Type") != "application/json" {
		t.Fatal(h)
	}
}
