// Package drimtest 是测试辅助（《server-go/2.接口设计》第 11 节）：假的 OpenAPI 服务、回调请求的构造与签名、签名的测试向量。
package drimtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/endpoints"
)

// Server 是假的 OpenAPI（httptest.Server）：可以按接口名设置返回值或错误，并记录收到的请求。
// 没有设置返回值的接口：有结果的返回 200 和 {}，没有结果的返回 204。
type Server struct {
	URL          string // 服务端地址，作为 drim.Config.BaseURL
	OrgName      string
	AppName      string
	ClientID     string
	ClientSecret string
	TokenTTL     time.Duration // 签发的 App Token 的有效期，默认 7 天

	srv      *httptest.Server
	mu       sync.Mutex
	rules    map[string][]*rule
	requests []Request
	tokens   map[string]bool
	issued   int
	routes   []route
}

// Request 是收到的一个请求。
type Request struct {
	Op     string // 接口名，如 users.create；换取令牌为 app.token
	Method string
	Path   string // 不含 /{org_name}/{app_name} 前缀
	Query  url.Values
	Header http.Header
	Body   []byte
}

// JSON 把请求体解析为 map（没有请求体时为 nil）。
func (r Request) JSON() map[string]any {
	if len(r.Body) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(r.Body, &out)
	return out
}

type rule struct {
	handler http.HandlerFunc
	times   int // 0 为一直有效
}

type route struct {
	ep      *endpoints.Endpoint
	pattern *regexp.Regexp
	params  int
}

// NewServer 启动假的 OpenAPI；用完调用 Close。
func NewServer() *Server {
	s := &Server{
		OrgName:      "1100250925",
		AppName:      "demo",
		ClientID:     "fake-client-id",
		ClientSecret: "fake-client-secret",
		TokenTTL:     7 * 24 * time.Hour,
		rules:        map[string][]*rule{},
		tokens:       map[string]bool{},
	}
	for _, ep := range endpoints.All {
		if ep.Internal {
			continue
		}
		parts := regexp.MustCompile(`\{[^}]+\}`).Split(ep.Path, -1)
		quoted := make([]string, len(parts))
		for i, p := range parts {
			quoted[i] = regexp.QuoteMeta(p)
		}
		s.routes = append(s.routes, route{ep, regexp.MustCompile("^" + strings.Join(quoted, "[^/]+") + "$"), len(parts) - 1})
	}
	// 路径参数少的（字面量多的）先匹配：/users/{u}/conversations/read-all 优先于 /users/{u}/conversations/{id}
	sort.SliceStable(s.routes, func(i, j int) bool { return s.routes[i].params < s.routes[j].params })
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	return s
}

// Close 关闭服务。
func (s *Server) Close() { s.srv.Close() }

// Config 返回连到这个服务的 drim.Config（不使用代理）。
func (s *Server) Config() drim.Config {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return drim.Config{
		BaseURL:      s.URL,
		OrgName:      s.OrgName,
		AppName:      s.AppName,
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		HTTPClient:   &http.Client{Transport: transport},
	}
}

// Option 是 Respond、Fail、Handle 的选项。
type Option func(*rule)

// Times 只生效 n 次（默认一直有效；后设置的先用）。
func Times(n int) Option { return func(r *rule) { r.times = n } }

// Handle 用自己的函数处理接口 op（如 users.create；换取令牌为 app.token）的请求。
func (s *Server) Handle(op string, handler http.HandlerFunc, opts ...Option) {
	r := &rule{handler: handler}
	for _, o := range opts {
		o(r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[op] = append([]*rule{r}, s.rules[op]...)
}

// Respond 设置接口 op 的返回值：status 和 body（[]byte、string 原样发送，其他值编码为 JSON；nil 为空的响应体）。
func (s *Server) Respond(op string, status int, body any, opts ...Option) {
	data := encodeBody(body)
	s.Handle(op, func(w http.ResponseWriter, _ *http.Request) {
		if len(data) > 0 {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}, opts...)
}

// FailOptions 是 Fail 的可选项。
type FailOptions struct {
	Status     int // 默认按错误码（如 rate_limited 为 429）
	Message    string
	Details    map[string]any
	RetryAfter string // Retry-After 响应头，如 "2"
	Times      int    // 默认 1 次；-1 为一直有效
}

// Fail 让接口 op 返回错误，如 Fail("messages.send", "rate_limited", FailOptions{RetryAfter: "2"})，默认一次。
func (s *Server) Fail(op, code string, opt FailOptions) {
	status := opt.Status
	if status == 0 {
		status = StatusOf(code)
	}
	body := ErrorBody(code, opt.Message, opt.Details)
	times := opt.Times
	if times == 0 {
		times = 1
	}
	if times < 0 {
		times = 0
	}
	s.Handle(op, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if opt.RetryAfter != "" {
			w.Header().Set("Retry-After", opt.RetryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}, Times(times))
}

// RevokeTokens 让已签发的 App Token 全部失效：之后的请求得到 unauthenticated，SDK 会换取新令牌后重发。
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

// Requests 返回收到的全部请求。
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls 返回收到的接口 op 的请求。
func (s *Server) Calls(op string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

var statusOf = map[string]int{
	"invalid_argument": 400, "unauthenticated": 401, "invalid_client": 401, "permission_denied": 403,
	"ip_not_allowed": 403, "tenant_unavailable": 403, "app_unavailable": 403, "user_disabled": 403,
	"not_group_member": 403, "group_disabled": 403, "chatroom_disabled": 403, "message_rejected": 403,
	"content_rejected": 403, "file_blocked": 403, "not_found": 404, "method_not_allowed": 405,
	"already_exists": 409, "version_conflict": 409, "limit_exceeded": 409, "file_expired": 410,
	"payload_too_large": 413, "rate_limited": 429, "internal": 500,
}

// StatusOf 返回错误码对应的 HTTP 状态码（不认识的为 400）。
func StatusOf(code string) int {
	if status, ok := statusOf[code]; ok {
		return status
	}
	return http.StatusBadRequest
}

// ErrorBody 返回 OpenAPI 的错误体 {"error":{...}}。
func ErrorBody(code, message string, details map[string]any) []byte {
	errObj := map[string]any{"code": code, "message": message, "request_id": "FAKEREQUESTID"}
	if details != nil {
		errObj["details"] = details
	}
	data, _ := json.Marshal(map[string]any{"error": errObj})
	return data
}

func encodeBody(body any) []byte {
	switch v := body.(type) {
	case nil:
		return nil
	case []byte:
		return v
	case string:
		return []byte(v)
	case json.RawMessage:
		return v
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + url.PathEscape(s.OrgName) + "/" + url.PathEscape(s.AppName)
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, prefix+"/") {
		writeError(w, "not_found", "接口不存在")
		return
	}
	sub := path[len(prefix):]
	body, _ := io.ReadAll(r.Body)
	recorded := Request{Method: r.Method, Path: sub, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if sub == "/token" && r.Method == http.MethodPost {
		recorded.Op = "app.token"
		s.mu.Lock()
		s.requests = append(s.requests, recorded)
		handler := s.take("app.token")
		s.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		s.issueToken(w, body)
		return
	}
	var ep *endpoints.Endpoint
	for _, rt := range s.routes {
		if rt.ep.Method == r.Method && rt.pattern.MatchString(sub) {
			ep = rt.ep
			break
		}
	}
	if ep == nil {
		writeError(w, "not_found", "接口不存在")
		return
	}
	recorded.Op = ep.Name
	s.mu.Lock()
	s.requests = append(s.requests, recorded)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	valid := s.tokens[token]
	handler := s.take(ep.Name)
	s.mu.Unlock()
	if !valid {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(ErrorBody("unauthenticated", "App Token 无效或已过期", nil))
		return
	}
	if handler != nil {
		handler(w, r)
		return
	}
	if !ep.HasResult {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// take 取出一个有效的规则（调用方持有锁）。
func (s *Server) take(op string) http.HandlerFunc {
	for _, r := range s.rules[op] {
		if r.times == 0 {
			return r.handler
		}
		if r.times > 0 {
			r.times--
			if r.times == 0 {
				r.times = -1 // 用完
			}
			return r.handler
		}
	}
	return nil
}

func (s *Server) issueToken(w http.ResponseWriter, body []byte) {
	var req struct {
		GrantType    string `json:"grant_type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	_ = json.Unmarshal(body, &req)
	if req.ClientID != s.ClientID || req.ClientSecret != s.ClientSecret || req.GrantType != "client_credentials" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(ErrorBody("invalid_client", "Client ID 或 Client Secret 错误", nil))
		return
	}
	s.mu.Lock()
	s.issued++
	token := fmt.Sprintf("fake-token-%d", s.issued)
	s.tokens[token] = true
	ttl := s.TokenTTL
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"` + token + `","expires_in":` + strconv.FormatInt(int64(ttl/time.Second), 10) +
		`,"application":"99579846509723648"}`))
}

func writeError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(StatusOf(code))
	_, _ = w.Write(ErrorBody(code, message, nil))
}
