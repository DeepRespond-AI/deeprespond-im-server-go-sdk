package drim

import (
	"context"
	"regexp"
	"time"
)

// 单次调用的选项通过 ctx 传入，不改变方法的签名；只影响用这个 ctx 发出的调用，遍历方法（All 等）的每一页都沿用。

type callOptions struct {
	requestID      string
	attemptTimeout time.Duration
	maxAttempts    int
	maxRetryAfter  time.Duration
	hasRetryAfter  bool
}

type optionsKey struct{}

func optionsFrom(ctx context.Context) callOptions {
	if v, ok := ctx.Value(optionsKey{}).(callOptions); ok {
		return v
	}
	return callOptions{}
}

func withOption(ctx context.Context, set func(*callOptions)) context.Context {
	opts := optionsFrom(ctx)
	set(&opts)
	return context.WithValue(ctx, optionsKey{}, opts)
}

// WithRequestID 作为 X-Request-ID 发送调用方的追踪 ID（服务端原样沿用并写入日志），同一次调用的重试沿用同一个 ID。
// 必须是 1 到 64 个字母、数字或 - _ .，不合规的在本地以 invalid_params 拒绝（服务端会把它换掉，日志就对不上了）。
func WithRequestID(ctx context.Context, id string) context.Context {
	return withOption(ctx, func(o *callOptions) { o.requestID = id })
}

// WithAttemptTimeout 这次调用中每次尝试的时限，覆盖 Config.Timeout（也覆盖完成上传的 150 秒）。
func WithAttemptTimeout(ctx context.Context, d time.Duration) context.Context {
	return withOption(ctx, func(o *callOptions) { o.attemptTimeout = d })
}

// WithMaxAttempts 这次调用中网络错误、超时、5xx 的最多尝试次数（含第一次），1 表示不自动重试；
// 限流后的等待重发仍按 RetryPolicy.MaxRateLimited。
func WithMaxAttempts(ctx context.Context, n int) context.Context {
	return withOption(ctx, func(o *callOptions) { o.maxAttempts = n })
}

// WithMaxRetryAfter 这次调用自动等待的 Retry-After 上限；0 表示被限流时不等待，直接返回 rate_limited。
func WithMaxRetryAfter(ctx context.Context, d time.Duration) context.Context {
	return withOption(ctx, func(o *callOptions) {
		o.maxRetryAfter = d
		o.hasRetryAfter = true
	})
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validRequestID(id string) bool { return requestIDPattern.MatchString(id) }
