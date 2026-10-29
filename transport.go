package drim

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/endpoints"
)

const (
	maxRequestBytes  = 1 << 20  // 请求体上限 1 MiB（本地检查）
	maxResponseBytes = 16 << 20 // 响应体上限 16 MB
	slowWaitWarning  = 5 * time.Second
	scopeClient      = "client"
)

// backoffBase 退避：0.5、1、2 秒，各乘以 0.5 到 1.5 的随机数（《1.功能概述文档》第 6.2 节）。
var backoffBase = [...]time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// transport 是一个客户端的发送逻辑：编码、重试、限流等待、令牌失效后重发、解码。各模块的方法不写重试循环。
type transport struct {
	prefix         string // {BaseURL}/{org_name}/{app_name}
	http           *http.Client
	ownsHTTP       bool
	timeout        time.Duration
	maxAttempts    int
	maxRateLimited int
	maxRetryAfter  time.Duration
	tokens         *tokenManager
	pauses         pauseBoard
	limiter        *rateLimiter
	stats          statsRecorder
	hooks          Hooks
	log            *slog.Logger
	userAgent      string
	closed         atomic.Bool
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// attemptFailure 是没有收到响应的一次尝试。
type attemptFailure struct {
	err       *Error
	sent      bool // 请求可能已经发出（服务端可能已经执行）
	retryable bool
	final     bool // 调用方取消、ctx 到期、响应过大等，不再重试
}

// do 发出一次调用并把结果解码到 out（nil 表示没有结果）。返回的错误都是 *Error。
func (t *transport) do(ctx context.Context, c *call, out any) error {
	ep := c.ep
	if c.err == nil && c.w != nil && c.w.err != nil {
		c.err = c.w.err
	}
	opts := optionsFrom(ctx)
	requestID := opts.requestID
	if c.err == nil && requestID != "" && !validRequestID(requestID) {
		c.err = invalidParams("request_id", "WithRequestID 的 ID 必须是 1 到 64 个字母、数字或 - _ .")
	}
	if requestID == "" || !validRequestID(requestID) {
		requestID = newRequestID()
	}
	if c.err != nil {
		return t.finish(ep, c.err, requestID, 0, false)
	}
	if t.closed.Load() {
		e := newError(CodeClientUnavailable, "客户端已关闭")
		e.Details = map[string]any{"cause": "closed"}
		return t.finish(ep, e, requestID, 0, false)
	}
	var body []byte
	hasBody := false
	if c.w != nil {
		body = c.w.buf
		hasBody = true
		if ep.EmptyBody && string(body) == "{}" {
			// 请求体可以省略的接口没有参数时发送长度为 0 的请求体（服务端按 Content-Length 为 0 判断）
			body, hasBody = nil, false
		}
		if len(body) > maxRequestBytes {
			e := newError(CodePayloadTooLarge, "请求体超过 1 MiB，没有发送")
			e.Details = map[string]any{"max_bytes": maxRequestBytes, "size": len(body)}
			return t.finish(ep, e, requestID, 0, false)
		}
	}
	res, attempts, err := t.send(ctx, c, body, hasBody, requestID, opts)
	if err != nil {
		return err
	}
	return t.decode(c, res, attempts, out, requestID)
}

func (t *transport) send(ctx context.Context, c *call, body []byte, hasBody bool, requestID string, opts callOptions) (*response, int, error) {
	ep := c.ep
	unsafe := ep.Category == endpoints.Unsafe || c.unsafe
	perAttempt := t.timeout
	if ep.Timeout > 0 {
		perAttempt = ep.Timeout
	}
	if opts.attemptTimeout > 0 {
		perAttempt = opts.attemptTimeout
	}
	maxAttempts, maxRateLimited := t.maxAttempts, t.maxRateLimited
	if opts.maxAttempts > 0 {
		maxAttempts = opts.maxAttempts
	}
	maxRetryAfter := t.maxRetryAfter
	if opts.hasRetryAfter {
		maxRetryAfter = opts.maxRetryAfter
	}
	target := t.prefix + c.urlPath
	if len(c.rawQuery) > 0 {
		target += "?" + string(c.rawQuery)
	}
	var (
		attempt, retries, rateLimited int
		ambiguous, refreshed          bool
		extraWait                     time.Duration
	)
	for {
		waited, werr := t.waitTurn(ctx, c, extraWait)
		extraWait = 0
		if werr != nil {
			return nil, attempt, t.finish(ep, werr, requestID, attempt, ambiguous && attempt > 0)
		}
		attempt++
		token, terr := t.tokens.get(ctx)
		if terr != nil {
			return nil, attempt, t.finish(ep, terr, requestID, attempt, ambiguous && attempt > 1)
		}
		res, failure, elapsed := t.attempt(ctx, ep, target, body, hasBody, token, requestID, perAttempt)
		if failure != nil {
			t.after(ctx, ep, target, attempt, 0, failure.err.Code, "", requestID, elapsed, waited)
			t.stats.attempt(ep.Name, attempt > 1, waited)
			if failure.sent {
				ambiguous = true
			}
			if !failure.final && failure.retryable && (!failure.sent || !unsafe) && retries < maxAttempts-1 {
				delay := backoff(retries, 0)
				if fitsDeadline(ctx, delay) {
					retries++
					t.log.Warn("请求失败，将要重试", "op", ep.Name, "attempt", attempt, "code", failure.err.Code, "delay", delay)
					if sleepCtx(ctx, delay) != nil {
						return nil, attempt, t.finish(ep, ctxError(ctx), requestID, attempt, ambiguous)
					}
					continue
				}
			}
			if !failure.final {
				if deadline, ok := ctx.Deadline(); ok && failure.err.Code == CodeTimeout && !time.Now().Before(deadline.Add(-time.Millisecond)) {
					failure.err.Message = "这次调用的时限已到"
				}
			}
			return nil, attempt, t.finish(ep, failure.err, requestID, attempt, ambiguous && attempt > 1)
		}
		responseID := res.header.Get("X-Request-ID")
		if responseID == "" {
			responseID = requestID
		}
		if res.status >= 200 && res.status < 300 {
			t.after(ctx, ep, target, attempt, res.status, "", "", responseID, elapsed, waited)
			t.stats.attempt(ep.Name, attempt > 1, waited)
			return res, attempt, nil
		}
		e := errorFromResponse(res.status, res.header, res.body, requestID)
		t.after(ctx, ep, target, attempt, res.status, e.Code, e.Reason(), e.RequestID, elapsed, waited)
		t.stats.attempt(ep.Name, attempt > 1, waited)
		switch {
		case e.Code == CodeUnauthenticated:
			reason := e.Reason()
			if reason == reasonAppDeleted || reason == reasonTenantClosed {
				t.tokens.markPermanent(e)
				return nil, attempt, t.finish(ep, e, requestID, attempt, false)
			}
			if reason == "" && !refreshed {
				// 令牌已失效：换取一次新令牌后重发这个请求一次
				refreshed = true
				t.tokens.invalidate(ctx, token)
				continue
			}
		case e.Code == CodeRateLimited:
			serverWait := e.RetryAfter
			if serverWait <= 0 {
				serverWait = defaultRateLimitedWait
			}
			wait := rateLimitWait(e)
			if serverWait > maxRetryAfter || rateLimited >= maxRateLimited || !fitsDeadline(ctx, wait) {
				break
			}
			if scope := t.rateScope(e.Reason(), ep); scope != "" {
				// 整个客户端（或这个分组）一起等待到同一时刻，而不是各自重试
				t.pauses.push(scope, time.Now().Add(wait))
			} else {
				extraWait = wait // 只针对某个群、聊天室、文件的限制：只有这个请求等待
			}
			rateLimited++
			t.log.Debug("被限流，等待后重发", "op", ep.Name, "reason", e.Reason(), "wait", wait)
			continue
		case e.temporary():
			ambiguous = true
			if !unsafe && retries < maxAttempts-1 {
				var hint time.Duration
				if e.RetryAfter > 0 && e.RetryAfter <= maxRetryAfter {
					hint = e.RetryAfter
				}
				delay := backoff(retries, hint)
				if fitsDeadline(ctx, delay) {
					retries++
					t.log.Warn("请求失败，将要重试", "op", ep.Name, "attempt", attempt, "status", res.status, "code", e.Code, "delay", delay)
					if sleepCtx(ctx, delay) != nil {
						return nil, attempt, t.finish(ep, ctxError(ctx), requestID, attempt, ambiguous)
					}
					continue
				}
			}
		}
		return nil, attempt, t.finish(ep, e, requestID, attempt, ambiguous && attempt > 1)
	}
}

// attempt 发出一次请求并读完响应（不超过 16 MB）。
func (t *transport) attempt(ctx context.Context, ep *endpoints.Endpoint, target string, body []byte, hasBody bool,
	token, requestID string, perAttempt time.Duration) (*response, *attemptFailure, time.Duration) {
	actx, cancel := context.WithTimeout(ctx, perAttempt)
	defer cancel()
	var sent atomic.Bool
	actx = httptrace.WithClientTrace(actx, &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }})
	req, err := http.NewRequestWithContext(actx, ep.Method, target, nil)
	if err != nil {
		return nil, &attemptFailure{err: invalidParams("", "请求地址不合法："+err.Error()), final: true}, 0
	}
	switch {
	case hasBody:
		// 不设置 GetBody：net/http 不会自行重发，重试全部由 SDK 按类别控制
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	case ep.Method == http.MethodGet:
		// GET 的请求体为空但不是 nil：net/http 不把它当作可以自行重发的请求（线上仍然没有请求体）
		req.Body = io.NopCloser(bytes.NewReader(nil))
		req.ContentLength = 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", t.userAgent)
	req.Header.Set("X-Request-ID", requestID)
	if t.hooks.BeforeRequest != nil {
		t.hooks.BeforeRequest(ctx, req)
	}
	started := time.Now()
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, classify(ctx, actx, err, sent.Load()), time.Since(started)
	}
	data, rerr := readLimited(resp.Body, maxResponseBytes)
	resp.Body.Close()
	elapsed := time.Since(started)
	if errors.Is(rerr, errTooLarge) {
		e := newError(CodeInvalidResponse, "响应体超过 16 MB")
		e.StatusCode = resp.StatusCode
		e.Details = map[string]any{"body_prefix": bodyPrefix(data)}
		return nil, &attemptFailure{err: e, sent: true, final: true}, elapsed
	}
	if rerr != nil {
		return nil, classify(ctx, actx, rerr, true), elapsed
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}, nil, elapsed
}

// classify 把没有收到完整响应的错误分类：调用方取消或到期、尝试超时、网络错误；请求是否可能已经发出。
func classify(ctx, actx context.Context, err error, sent bool) *attemptFailure {
	if ctx.Err() != nil {
		return &attemptFailure{err: ctxError(ctx), sent: sent, final: true}
	}
	if errors.Is(actx.Err(), context.DeadlineExceeded) {
		e := newError(CodeTimeout, "请求超时")
		e.Err = context.DeadlineExceeded
		return &attemptFailure{err: e, sent: sent, retryable: true}
	}
	e := newError(CodeNetworkError, "")
	e.Err = err
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		e.Message = "证书校验失败：" + err.Error()
		return &attemptFailure{err: e, final: true}
	}
	if errors.As(err, &record) {
		e.Message = "TLS 握手失败（地址的协议不对？）：" + err.Error()
		return &attemptFailure{err: e, final: true}
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	if errors.As(err, &dnsErr) || (errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect")) {
		sent = false
	}
	if sent {
		e.Message = "连接中断：" + err.Error()
	} else {
		e.Message = "连接失败：" + err.Error()
	}
	return &attemptFailure{err: e, sent: sent, retryable: true}
}

// ctxError 是 ctx 已结束的错误：取消为 canceled，到期为 timeout。
func ctxError(ctx context.Context) *Error {
	cause := ctx.Err()
	if errors.Is(cause, context.Canceled) {
		e := newError(CodeCanceled, "调用方已取消")
		e.Err = cause
		return e
	}
	e := newError(CodeTimeout, "这次调用的时限已到")
	e.Err = context.DeadlineExceeded
	return e
}

var errTooLarge = errors.New("响应体超过上限")

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > limit {
		return data, errTooLarge
	}
	return data, nil
}

// decode 把成功的响应解码到 out，并按接口整理结果（状态码的标记、带版本号的列表等）。
func (t *transport) decode(c *call, res *response, attempts int, out any, requestID string) error {
	if out == nil {
		return nil
	}
	body := beforeDecode(c.ep, res.body)
	responseID := res.header.Get("X-Request-ID")
	if responseID == "" {
		responseID = requestID
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		e := newError(CodeInvalidResponse, "响应不是 JSON 对象")
		e.StatusCode = res.status
		e.Details = map[string]any{"body_prefix": bodyPrefix(res.body)}
		e.RequestID = responseID
		return t.finish(c.ep, e, requestID, attempts, false)
	}
	if err := json.Unmarshal(body, out); err != nil {
		e := newError(CodeInvalidResponse, "响应不是预期的 JSON："+err.Error())
		e.StatusCode = res.status
		e.Details = map[string]any{"body_prefix": bodyPrefix(res.body)}
		e.RequestID = responseID
		e.Err = err
		return t.finish(c.ep, e, requestID, attempts, false)
	}
	afterDecode(c.ep, out, res.status)
	if c.after != nil {
		c.after(out, attempts)
	}
	return nil
}

// finish 补全错误的接口名、尝试次数和请求 ID，记统计和日志。换取令牌的错误保留接口名 app.token。
func (t *transport) finish(ep *endpoints.Endpoint, e *Error, requestID string, attempts int, retried bool) *Error {
	if e.Op == "" {
		e.Op = ep.Name
	}
	if e.Attempts == 0 {
		e.Attempts = max(attempts, 1)
	}
	e.Retried = e.Retried || retried
	if e.RequestID == "" {
		e.RequestID = requestID
	}
	t.stats.failure(ep.Name)
	if loudError(e) {
		t.log.Error("请求失败", "op", e.Op, "method", ep.Method, "path", ep.Path, "status", e.StatusCode, "code", e.Code,
			"reason", e.Reason(), "request_id", e.RequestID, "attempts", e.Attempts)
	} else {
		t.log.Debug("请求失败", "op", e.Op, "status", e.StatusCode, "code", e.Code, "reason", e.Reason(), "request_id", e.RequestID)
	}
	return e
}

// loudError 最终失败时记错误日志的：4xx 中只有 401、ip_not_allowed、405，其余是调用方的业务结果（《1.功能概述文档》第 13 节）。
func loudError(e *Error) bool {
	switch e.Code {
	case CodeNetworkError, CodeTimeout, CodeInvalidResponse, CodeClientUnavailable, CodeUnauthenticated,
		CodeIPNotAllowed, CodeMethodNotAllowed, CodeInternal:
		return true
	}
	return e.StatusCode >= 500
}

func (t *transport) after(ctx context.Context, ep *endpoints.Endpoint, target string, attempt, status int, code, reason, requestID string,
	elapsed, waited time.Duration) {
	if t.hooks.AfterResponse == nil {
		return
	}
	plain, _, _ := strings.Cut(target, "?")
	t.hooks.AfterResponse(ctx, RequestInfo{
		Op: ep.Name, Method: ep.Method, URL: plain, Attempt: attempt, StatusCode: status, Code: code, Reason: reason,
		RequestID: requestID, Duration: elapsed, RateLimitWait: waited,
	})
}

// ---- 限流 ----

// rateScope 按 details.reason 决定等待的范围（《1.功能概述文档》第 6.3 节）：没有原因为整个客户端；
// app_message_rate、request_rate 为接口的分组（发送消息、代用户举报）；其他只有这个请求。
func (t *transport) rateScope(reason string, ep *endpoints.Endpoint) string {
	switch reason {
	case "":
		return scopeClient
	case "app_message_rate", "request_rate":
		if ep.RateGroup != "" {
			return ep.RateGroup
		}
	}
	return ""
}

// rateLimitWait Retry-After（没有时 1 秒）加 0 到 20% 的随机量；app_message_rate 在 Retry-After 到它的 2 倍之间。
func rateLimitWait(e *Error) time.Duration {
	base := e.RetryAfter
	if base <= 0 {
		base = defaultRateLimitedWait
	}
	if e.Reason() == "app_message_rate" {
		return time.Duration(float64(base) * (1 + mathrand.Float64()))
	}
	return time.Duration(float64(base) * (1 + 0.2*mathrand.Float64()))
}

func backoff(retry int, atLeast time.Duration) time.Duration {
	base := backoffBase[min(retry, len(backoffBase)-1)]
	d := time.Duration(float64(base) * (0.5 + mathrand.Float64()))
	return max(d, atLeast)
}

// waitTurn 等到限流的暂停时刻和本地限速允许；返回等待的总时长。等待会超过 ctx 的截止时间时不等待，直接返回 rate_limited。
func (t *transport) waitTurn(ctx context.Context, c *call, extra time.Duration) (time.Duration, *Error) {
	if ctx.Err() != nil {
		return 0, ctxError(ctx)
	}
	ep := c.ep
	var waited time.Duration
	for {
		wait := t.pauses.wait(scopeClient, ep.RateGroup)
		if extra > wait {
			wait = extra
		}
		extra = 0
		if wait <= 0 {
			break
		}
		if !fitsDeadline(ctx, wait) {
			e := newError(CodeRateLimited, "客户端处于限流等待中，等待会超过这次调用的时限")
			e.RetryAfter = wait
			return waited, e
		}
		if wait > slowWaitWarning {
			t.log.Warn("限流等待超过 5 秒", "op", ep.Name, "wait", wait)
		}
		if sleepCtx(ctx, wait) != nil {
			return waited, ctxError(ctx)
		}
		waited += wait
	}
	if t.limiter != nil {
		cost := 1
		if ep.BatchCharged && c.items > 1 {
			cost = c.items
		}
		if wait := t.limiter.reserve(cost); wait > 0 {
			if !fitsDeadline(ctx, wait) {
				e := newError(CodeTimeout, "本地限速的等待会超过这次调用的时限")
				e.Err = context.DeadlineExceeded
				return waited, e
			}
			if sleepCtx(ctx, wait) != nil {
				return waited, ctxError(ctx)
			}
			waited += wait
		}
	}
	return waited, nil
}

func fitsDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Now().Add(wait).Before(deadline)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pauseBoard 是“暂停到某个时刻”：整个客户端一个，每个分组（发送消息、代用户举报）一个；只往后推。
type pauseBoard struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (p *pauseBoard) push(key string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until == nil {
		p.until = map[string]time.Time{}
	}
	if until.After(p.until[key]) {
		p.until[key] = until
	}
}

func (p *pauseBoard) wait(keys ...string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	var latest time.Time
	for _, key := range keys {
		if key == "" {
			continue
		}
		if u := p.until[key]; u.After(latest) {
			latest = u
		}
	}
	if latest.IsZero() {
		return 0
	}
	return time.Until(latest)
}

// rateLimiter 是调用方自己的本地限速（令牌桶）：批量接口按项数计，最多扣到桶的容量。
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	stamp  time.Time
}

func newRateLimiter(cfg *RateLimit) *rateLimiter {
	burst := float64(cfg.Burst)
	if burst <= 0 {
		burst = math.Max(1, math.Ceil(cfg.PerSecond))
	}
	return &rateLimiter{rate: cfg.PerSecond, burst: burst, tokens: burst, stamp: time.Now()}
}

// reserve 预订 cost 个令牌，返回需要等待的时长（预订后令牌数可以为负，表示欠下的）。
func (l *rateLimiter) reserve(cost int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.stamp).Seconds()*l.rate)
	l.stamp = now
	l.tokens -= math.Min(float64(cost), l.burst)
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

// statsRecorder 按接口名统计。
type statsRecorder struct {
	mu  sync.Mutex
	ops map[string]*OpStats
}

func (s *statsRecorder) get(op string) *OpStats {
	if s.ops == nil {
		s.ops = map[string]*OpStats{}
	}
	stats := s.ops[op]
	if stats == nil {
		stats = &OpStats{}
		s.ops[op] = stats
	}
	return stats
}

func (s *statsRecorder) attempt(op string, retry bool, waited time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.get(op)
	stats.Requests++
	if retry {
		stats.Retries++
	}
	stats.RateLimitWait += waited
}

func (s *statsRecorder) failure(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(op).Failures++
}

func (s *statsRecorder) snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Ops: make(map[string]OpStats, len(s.ops))}
	for op, stats := range s.ops {
		out.Ops[op] = *stats
	}
	return out
}

// newRequestID 生成 26 个字符的 X-Request-ID（base32，与服务端生成的格式相同）。
func newRequestID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	var b [26]byte
	var random [26]byte
	_, _ = rand.Read(random[:])
	for i := range b {
		b[i] = alphabet[random[i]&31]
	}
	return string(b[:])
}

// newClientMsgID 生成 32 个小写十六进制字符的 client_msg_id（调用方没有给出时，使发送可以安全重试）。
func newClientMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
