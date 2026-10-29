// Package callback 处理回调请求（《server-go/2.接口设计》第 9、10 节）：校验签名、防重放、按事件 ID 去重、
// 回应 URL 验证、分派事件回调和同步回调。Handler 实现 http.Handler，可以挂到标准库的 ServeMux、Gin（gin.WrapH）、
// Echo（echo.WrapHandler）、chi 上；注意不要加会读取或改写请求体的中间件，签名按原始字节计算。
package callback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	nonceTTL                  = 10 * time.Minute
	maxRetryAfter             = time.Hour
	hookMargin                = 50 * time.Millisecond
	defaultMaxBodyBytes       = 2 << 20
	defaultClaimTTL           = 30 * time.Second
	defaultMaxConcurrentHooks = 100
)

// Config 是回调处理器的配置。
type Config struct {
	Secrets SecretProvider // 按地址 ID 返回密钥列表（轮换期间新旧两个）
	Nonces  NonceStore     // 随机数 10 分钟去重；多实例部署时用共享存储
	Dedup   DedupStore     // 事件 ID 去重；多实例部署或需要重新投递几天前的事件时用共享存储
	Events  EventHandlers  // 同步方式：在请求中调用处理函数
	Queue   EventQueue     // 异步方式：交给调用方的持久队列后立即返回；与 Events 二选一
	Hooks   HookHandlers   // 同步回调的处理函数
	// HookFallback 同步回调处理超时、出错或结果不合规时的结果；nil 表示返回 503，由服务端按应用的失败策略处理。
	// 为 Allow() 时出错和超时都放行（不依赖服务端的失败策略，日志照常记录）；503 计入服务端的熔断统计。
	HookFallback *Verdict
	// ClaimTTL 事件处理的占用时限，默认 30 秒；超过后同一事件可以在别的请求中重新处理。
	ClaimTTL time.Duration
	// MaxBodyBytes 请求体的上限，默认 2 MB；超过的返回 413。
	MaxBodyBytes int64
	// MaxConcurrentHooks 同时运行的同步回调处理函数的上限，默认 100；超时后不等的处理函数仍在运行时占用名额，
	// 已满时直接按 HookFallback（或 503）返回。
	MaxConcurrentHooks int
	// Clock 当前时间（测试用），默认 time.Now。
	Clock func() time.Time
	// Logger 默认 slog.Default()。签名校验失败、同步回调的结果不合规记错误日志，调用方应据此告警。
	Logger *slog.Logger
}

// SecretProvider 按地址 ID 返回这个地址的密钥列表（轮换期间新旧两个）；找不到时返回空列表，按校验失败处理。
type SecretProvider interface {
	Secrets(ctx context.Context, endpointID string) ([]string, error)
}

// StaticSecrets 是固定的“地址 ID → 密钥列表”。
type StaticSecrets map[string][]string

// Secrets 实现 SecretProvider。
func (s StaticSecrets) Secrets(_ context.Context, endpointID string) ([]string, error) {
	return s[endpointID], nil
}

// SecretFunc 把函数转为 SecretProvider（如从配置中心读取）。
type SecretFunc func(ctx context.Context, endpointID string) ([]string, error)

// Secrets 实现 SecretProvider。
func (f SecretFunc) Secrets(ctx context.Context, endpointID string) ([]string, error) {
	return f(ctx, endpointID)
}

// EventQueue 是异步方式的队列：入队成功即把这个事件记为已完成（之后的处理和重试由调用方的队列负责，必须是持久的）；
// 返回错误时释放占用，整批返回 503，服务端稍后重发。
type EventQueue interface {
	Enqueue(ctx context.Context, ev *Event) error
}

// RetryAfter 包装事件处理函数的错误：这一批返回 503 并带 Retry-After（取一批中最长的，不超过 1 小时），服务端按它推迟重发。
func RetryAfter(d time.Duration, err error) error {
	return &retryAfterError{d: d, err: err}
}

type retryAfterError struct {
	d   time.Duration
	err error
}

func (e *retryAfterError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("%v 后重试", e.d)
	}
	return e.err.Error()
}

func (e *retryAfterError) Unwrap() error { return e.err }

// Handler 处理回调请求。
type Handler struct {
	cfg   Config
	log   *slog.Logger
	slots chan struct{}
}

// NewHandler 检查配置并创建处理器。
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Secrets == nil {
		return nil, errors.New("callback: Secrets 不能为空")
	}
	if cfg.Nonces == nil || cfg.Dedup == nil {
		return nil, errors.New("callback: Nonces 和 Dedup 不能为空（单个实例可以用 NewMemoryNonceStore、NewMemoryDedupStore）")
	}
	if cfg.Queue != nil && !cfg.Events.empty() {
		return nil, errors.New("callback: Events 和 Queue 二选一：已经配置了 Queue（异步方式），事件交给队列，不能再设置事件处理函数")
	}
	if cfg.HookFallback != nil && cfg.HookFallback.action == "" {
		return nil, errors.New("callback: HookFallback 必须是 Allow()、Reject() 等结果")
	}
	if cfg.ClaimTTL < 0 || cfg.MaxBodyBytes < 0 || cfg.MaxConcurrentHooks < 0 {
		return nil, errors.New("callback: 时长和上限不能为负数")
	}
	if cfg.ClaimTTL == 0 {
		cfg.ClaimTTL = defaultClaimTTL
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxConcurrentHooks == 0 {
		cfg.MaxConcurrentHooks = defaultMaxConcurrentHooks
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{cfg: cfg, log: logger.With("component", "drim.callback"), slots: make(chan struct{}, cfg.MaxConcurrentHooks)}, nil
}

// ServeHTTP 实现 http.Handler：读取原始请求体（最多 MaxBodyBytes）后交给 HandleRequest。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeResponse(w, errorResponse(http.StatusMethodNotAllowed, "method not allowed", 0))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	if err != nil {
		h.log.Warn("读取回调请求体失败", "error", err)
		writeResponse(w, errorResponse(http.StatusBadRequest, "bad request", 0))
		return
	}
	if int64(len(body)) > h.cfg.MaxBodyBytes {
		writeResponse(w, errorResponse(http.StatusRequestEntityTooLarge, "payload too large", 0))
		return
	}
	status, header, respBody := h.HandleRequest(r.Context(), r.Header, body)
	writeResponse(w, result{status, header, respBody})
}

type result struct {
	status int
	header http.Header
	body   []byte
}

func writeResponse(w http.ResponseWriter, res result) {
	for key, values := range res.header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(res.status)
	if len(res.body) > 0 {
		_, _ = w.Write(res.body)
	}
}

// HandleRequest 是与框架无关的入口（云函数等自己读取请求的场景）：header 为请求头，body 为原始请求体。
func (h *Handler) HandleRequest(ctx context.Context, header http.Header, body []byte) (status int, respHeader http.Header, respBody []byte) {
	res := h.handle(ctx, header, body)
	return res.status, res.header, res.body
}

var hints = map[string]string{
	ReasonBadSignature:   "检查这个地址的密钥配置（轮换期间新旧两个都要配置），以及请求体是否被框架、中间件或代理改动过（必须用原始字节）",
	ReasonBadTimestamp:   "时间戳不是纯数字",
	ReasonStaleTimestamp: "时间戳与本机时间相差超过 5 分钟，检查本机时钟",
	ReasonBadFormat:      "缺少签名相关的请求头或格式不对",
}

func (h *Handler) handle(ctx context.Context, header http.Header, body []byte) result {
	if int64(len(body)) > h.cfg.MaxBodyBytes {
		return errorResponse(http.StatusRequestEntityTooLarge, "payload too large", 0)
	}
	endpointID := headerValue(header, "X-IM-Endpoint-Id")
	secrets, err := h.cfg.Secrets.Secrets(ctx, endpointID)
	if err != nil {
		h.log.Error("取得回调地址的密钥时出错", "endpoint_id", endpointID, "error", err)
		return errorResponse(http.StatusServiceUnavailable, "secret provider failed", 0)
	}
	if len(secrets) == 0 {
		h.log.Error("回调请求的地址 ID 不在配置中，检查 Secrets", "endpoint_id", endpointID)
		return errorResponse(http.StatusUnauthorized, "unknown endpoint", 0)
	}
	if err := Verify(secrets, header, body, h.cfg.Clock()); err != nil {
		reason := ReasonBadSignature
		var verr *VerifyError
		if errors.As(err, &verr) {
			reason = verr.Reason
		}
		h.log.Error("回调请求的签名校验失败", "endpoint_id", endpointID, "reason", reason, "hint", hints[reason])
		return errorResponse(http.StatusUnauthorized, reason, 0)
	}
	req, err := Parse(body)
	if err != nil {
		h.log.Error("回调请求体不合规", "endpoint_id", endpointID, "error", err)
		return errorResponse(http.StatusBadRequest, "bad request", 0)
	}
	if req.EndpointID != endpointID {
		h.log.Error("回调请求体中的 endpoint_id 与请求头不同", "header", endpointID, "body", req.EndpointID)
		return errorResponse(http.StatusUnauthorized, "endpoint mismatch", 0)
	}
	seen, err := h.cfg.Nonces.Seen(ctx, headerValue(header, "X-IM-Nonce"), nonceTTL)
	if err != nil {
		h.log.Error("随机数存储出错，返回 503（不能确认是否重放）", "error", err)
		return errorResponse(http.StatusServiceUnavailable, "nonce store failed", 0)
	}
	if seen {
		h.log.Error("回调请求的随机数重复（可能是重放）", "endpoint_id", endpointID, "request_id", req.RequestID)
		return errorResponse(http.StatusUnauthorized, "replayed", 0)
	}
	ctx = withRequest(ctx, req)
	switch req.Kind {
	case "verify":
		return jsonResponse(http.StatusOK, map[string]string{"challenge": req.Challenge})
	case "hook":
		return h.handleHook(ctx, req)
	default:
		return h.handleEvents(ctx, req)
	}
}

// handleEvents 逐个事件：占用 → 处理 → 完成（失败时释放）。有一个失败（处理出错、被并发占用、入队失败）就停止，
// 整批返回 503，服务端重发整批时已完成的被跳过。重复的事件（已完成）跳过并算作成功。
func (h *Handler) handleEvents(ctx context.Context, req *Request) result {
	for _, ev := range req.Events {
		state, token, err := h.cfg.Dedup.Claim(ctx, ev.ID, h.cfg.ClaimTTL)
		if err != nil {
			h.log.Error("去重存储出错（Claim），这一批返回 503", "event_id", ev.ID, "error", err)
			return errorResponse(http.StatusServiceUnavailable, "dedup store failed", 0)
		}
		switch state {
		case Done:
			h.log.Debug("跳过已处理的事件", "event_id", ev.ID, "type", ev.Type)
			continue
		case Acquired:
		default:
			h.log.Info("事件正在另一个请求中处理，这一批返回 503", "event_id", ev.ID, "type", ev.Type)
			return errorResponse(http.StatusServiceUnavailable, "event busy", 0)
		}
		started := time.Now()
		perr := h.process(ctx, ev)
		if elapsed := time.Since(started); elapsed > h.cfg.ClaimTTL {
			h.log.Warn("事件的处理超过了占用时限，同一事件可能已在别的请求中重新处理", "event_id", ev.ID, "elapsed", elapsed)
		}
		if perr != nil {
			var wait time.Duration
			var ra *retryAfterError
			if errors.As(perr, &ra) {
				wait = ra.d
			}
			h.log.Error("处理事件出错，这一批返回 503", "event_id", ev.ID, "type", ev.Type, "error", perr)
			if rerr := h.cfg.Dedup.Release(ctx, ev.ID, token); rerr != nil {
				h.log.Error("去重存储出错（Release），事件要等占用到期后才能重新处理", "event_id", ev.ID, "error", rerr)
			}
			return errorResponse(http.StatusServiceUnavailable, "retry later", wait)
		}
		if cerr := h.cfg.Dedup.Complete(ctx, ev.ID, token); cerr != nil {
			h.log.Error("去重存储出错（Complete），事件可能被重复处理", "event_id", ev.ID, "error", cerr)
		}
	}
	return result{status: http.StatusNoContent}
}

func (h *Handler) process(ctx context.Context, ev *Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("事件处理函数 panic", "event_id", ev.ID, "type", ev.Type, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("callback: 处理函数 panic：%v", r)
		}
	}()
	if h.cfg.Queue != nil {
		return h.cfg.Queue.Enqueue(ctx, ev)
	}
	if h.cfg.Events.Any != nil {
		if err := h.cfg.Events.Any(ctx, ev); err != nil {
			return err
		}
	}
	known, err := h.cfg.Events.dispatch(ctx, ev)
	if !known {
		if h.cfg.Events.Unknown != nil {
			return h.cfg.Events.Unknown(ctx, ev)
		}
		h.log.Debug("忽略不认识的事件", "event_id", ev.ID, "type", ev.Type)
	}
	return err
}

type hookOutcome struct {
	verdict Verdict
	err     error
}

// handleHook 在独立的协程中调用处理函数，截止时间为 timeout_ms 减 50 毫秒；到期时不等它结束，直接按 HookFallback 或 503 返回。
func (h *Handler) handleHook(ctx context.Context, req *Request) result {
	joinedVia, targets := joinInfo(req)
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}
	base := HookBase{Test: req.Test, Timeout: timeout}
	call, ok, err := h.cfg.Hooks.handler(req.Hook, req.Data, base)
	if err != nil {
		h.log.Error("同步回调的请求数据不合规", "hook", req.Hook, "error", err)
		return h.fallback(req.Hook, joinedVia, targets)
	}
	if !ok {
		h.log.Error("没有设置这个同步回调的处理函数", "hook", req.Hook)
		return h.fallback(req.Hook, joinedVia, targets)
	}
	select {
	case h.slots <- struct{}{}:
	default:
		h.log.Warn("同时运行的同步回调处理函数已达上限，直接按默认结果返回", "hook", req.Hook)
		return h.fallback(req.Hook, joinedVia, targets)
	}
	hctx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout-hookMargin))
	defer cancel()
	done := make(chan hookOutcome, 1)
	var abandoned atomic.Bool
	started := time.Now()
	go func() {
		defer func() { <-h.slots }()
		var outcome hookOutcome
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.log.Error("同步回调的处理函数 panic", "hook", req.Hook, "panic", r, "stack", string(debug.Stack()))
					outcome.err = fmt.Errorf("callback: 处理函数 panic：%v", r)
				}
			}()
			outcome.verdict, outcome.err = call(hctx)
		}()
		if abandoned.Load() {
			h.log.Warn("同步回调的处理函数在时限之后才结束，结果已丢弃", "hook", req.Hook, "elapsed", time.Since(started))
		}
		done <- outcome
	}()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			h.log.Error("同步回调的处理函数出错", "hook", req.Hook, "error", outcome.err)
			return h.fallback(req.Hook, joinedVia, targets)
		}
		body, err := encodeVerdict(outcome.verdict, req.Hook, joinedVia, targets)
		if err != nil {
			h.log.Error("同步回调的处理函数返回的结果不合规", "hook", req.Hook, "error", err)
			return h.fallback(req.Hook, joinedVia, targets)
		}
		return hookResponse(body)
	case <-hctx.Done():
		abandoned.Store(true)
		h.log.Warn("同步回调的处理函数超过时限，按默认结果返回", "hook", req.Hook, "timeout", timeout-hookMargin)
		return h.fallback(req.Hook, joinedVia, targets)
	}
}

func (h *Handler) fallback(hook, joinedVia string, targets []string) result {
	if h.cfg.HookFallback != nil {
		body, err := encodeVerdict(*h.cfg.HookFallback, hook, joinedVia, targets)
		if err == nil {
			return hookResponse(body)
		}
		h.log.Error("HookFallback 不适用于这个同步回调", "hook", hook, "error", err)
	}
	return errorResponse(http.StatusServiceUnavailable, "hook failed", 0)
}

func joinInfo(req *Request) (string, []string) {
	if req.Hook != "group.before_join" {
		return "", nil
	}
	var data struct {
		JoinedVia string   `json:"joined_via"`
		Targets   []string `json:"targets"`
	}
	_ = json.Unmarshal(req.Data, &data)
	return data.JoinedVia, data.Targets
}

func jsonResponse(status int, value any) result {
	body, _ := json.Marshal(value)
	return result{status: status, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, body: body}
}

func errorResponse(status int, message string, retryAfter time.Duration) result {
	res := jsonResponse(status, map[string]string{"error": message})
	if retryAfter > 0 {
		retryAfter = min(retryAfter, maxRetryAfter)
		res.header.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	return res
}

func hookResponse(body []byte) result {
	return result{status: http.StatusOK, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, body: body}
}
