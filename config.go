package drim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Config 是创建客户端的配置（《2.接口设计》第 3 节）。一个客户端对应一个应用。
type Config struct {
	BaseURL      string // 服务端地址，如 https://im.example.com；可以带路径前缀，不能带查询参数和片段
	OrgName      string
	AppName      string
	ClientID     string
	ClientSecret string

	// HTTPClient 默认由 SDK 创建（每个主机最多 100 个空闲连接、空闲 90 秒关闭、TLS 握手 10 秒、使用环境变量的代理、
	// 不跟随重定向）；Close 时关闭它的空闲连接。调用方传入的，SDK 不修改也不关闭。
	HTTPClient *http.Client
	// Timeout 每次尝试的时限（从发出到读完响应），默认 30 秒；完成上传为 150 秒，不受它影响。
	Timeout time.Duration
	// Retry 自动重试的设置（《1.功能概述文档》第 6 节）。
	Retry RetryPolicy
	// MaxRetryAfter 自动等待的 Retry-After 上限，默认 60 秒；超过的不等待，直接返回 rate_limited。
	MaxRetryAfter time.Duration
	// RateLimit 本地限速，默认关闭；用于批量导入、群发等后台任务，把额度留给线上请求。
	RateLimit *RateLimit
	// TokenStore 让多个服务实例共用一个 App Token；默认只在进程内存中。
	TokenStore TokenStore
	// Logger 默认不输出。
	Logger *slog.Logger
	// Hooks 请求前后的钩子（注入追踪头、记录指标）。
	Hooks Hooks
	// UserAgentSuffix 附加到 User-Agent 之后，如 "order-svc/2.3"。
	UserAgentSuffix string
}

// RetryPolicy 自动重试的设置。
type RetryPolicy struct {
	// MaxAttempts 网络错误、超时、5xx 的最多尝试次数（含第一次），默认 4；1 表示不重试。
	MaxAttempts int
	// MaxRateLimited rate_limited 后等待重发的次数，默认 3。
	MaxRateLimited int
	// Disabled 两种都不重试（不幂等的写操作本来就不重试）；令牌失效后换取一次再重发不受影响。
	Disabled bool
}

// RateLimit 本地限速（令牌桶）。批量接口按项数计（与服务端的额度计算一致），一个请求最多扣到桶的容量。
type RateLimit struct {
	PerSecond float64 // 每秒的请求数
	Burst     int     // 桶的容量，默认 max(1, ceil(PerSecond))
}

// TokenStore 让多个服务实例共用一个 App Token。Get 返回 ok 为假时由本实例换取后 Set。
// 存储出错时 SDK 退回本进程换取，不影响请求（记警告日志）。键为 drim:token:{16 个十六进制字符}，不含 Secret。
type TokenStore interface {
	Get(ctx context.Context, key string) (token string, expiresAt time.Time, ok bool, err error)
	Set(ctx context.Context, key string, token string, expiresAt time.Time) error
	// CompareAndDelete 只在存储中的令牌等于 token 时删除（避免删掉别的实例刚换来的新令牌）。
	CompareAndDelete(ctx context.Context, key string, token string) error
}

// Hooks 请求前后的钩子，在发出请求的协程中同步调用，不能阻塞太久；换取令牌的请求同样调用（接口名 app.token）。
// BeforeRequest 看到的请求中 Authorization 和请求体照常存在，钩子不应记录它们。钩子 panic 时 SDK 不恢复。
type Hooks struct {
	BeforeRequest func(ctx context.Context, req *http.Request)
	AfterResponse func(ctx context.Context, info RequestInfo)
}

// RequestInfo 是一次尝试的结果，交给 Hooks.AfterResponse。
type RequestInfo struct {
	Op            string        // 接口名，如 users.create；换取令牌为 app.token
	Method        string        // HTTP 方法
	URL           string        // 不含查询参数的地址
	Attempt       int           // 第几次尝试，从 1 开始
	StatusCode    int           // 没有收到响应时为 0
	Code          string        // 错误码（服务端的或 network_error、timeout 等），成功时为空
	Reason        string        // details.reason
	RequestID     string        // 响应头 X-Request-ID 或错误体中的，没有时为 SDK 发出的
	Duration      time.Duration // 这次尝试的耗时
	RateLimitWait time.Duration // 这次尝试之前限流等待（含本地限速）的时长
}

// Stats 是按接口名的统计（Client.Stats 返回的快照）。
type Stats struct {
	Ops map[string]OpStats
}

// OpStats 是一个接口的统计。
type OpStats struct {
	Requests      int64         // 尝试的次数（含重试）
	Failures      int64         // 最终失败的调用数
	Retries       int64         // 重试的次数
	RateLimitWait time.Duration // 限流等待的总时长
}

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxAttempts    = 4
	defaultMaxRateLimited = 3
	defaultMaxRetryAfter  = 60 * time.Second
)

// normalizeBaseURL 检查 BaseURL：http 或 https 的绝对地址、没有查询参数和片段；http 只允许本机和内网地址。
func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("drim: BaseURL 不是合法的地址：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("drim: BaseURL 必须是 http 或 https 的绝对地址")
	}
	if u.Host == "" {
		return "", errors.New("drim: BaseURL 缺少主机名")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("drim: BaseURL 不能带查询参数和片段")
	}
	if u.User != nil {
		return "", errors.New("drim: BaseURL 不能带用户名和密码")
	}
	if u.Scheme == "http" && !privateHost(u.Hostname()) {
		return "", errors.New("drim: http:// 只允许本机和内网地址，其他地址必须用 https（避免凭据以明文发出）")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func privateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return true
	}
	for _, prefix := range privatePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func defaultHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		// OpenAPI 不重定向；跟随重定向可能把请求发到别处，按收到的响应处理（invalid_response）
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// discardHandler 不输出任何日志（Go 1.23 没有 slog.DiscardHandler）。
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
