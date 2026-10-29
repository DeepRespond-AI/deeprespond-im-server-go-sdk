package drim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// App Token 的换取与缓存（《1.功能概述文档》第 3 节）：
//   - 单飞：同一客户端中多个请求同时需要新令牌时只发一次换取请求，其他请求等待它的结果；
//   - 剩余有效期不到 10%（最少 5 分钟）时在后台换取，换取期间仍用旧令牌；剩余不足 1 分钟时请求等待换取；
//   - 换取失败按错误码进入冷却：凭据错误、IP 不在白名单 1 分钟，服务暂停 2 到 5 分钟，冷却期内的请求直接返回 client_unavailable；
//   - app_deleted、tenant_closed 后客户端永久失效。
const (
	opToken         = "app.token"
	refreshMinAhead = 5 * time.Minute
	blockingAhead   = time.Minute
	storeTimeout    = 5 * time.Second
	maxTokenBody    = 1 << 20
)

var exchangeDelays = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

type tokenManager struct {
	mu            sync.Mutex
	clientID      string
	clientSecret  string
	gen           uint64 // 凭据的版本：更新凭据后，之前发出的换取结果不再采用
	token         string
	expiresAt     time.Time
	lifetime      time.Duration
	appID         string
	cooldownUntil time.Time
	cooldownErr   *Error
	permanent     *Error
	flight        *tokenFlight
	closed        bool

	tokenURL      string
	keyBase       [3]string // 服务端地址、org_name、app_name（令牌存储的键）
	storeKey      string
	store         TokenStore
	http          *http.Client
	userAgent     string
	timeout       time.Duration
	maxRetryAfter time.Duration
	hooks         Hooks
	log           *slog.Logger
	baseCtx       context.Context
	cancel        context.CancelFunc
}

type tokenFlight struct {
	done  chan struct{}
	token string
	err   *Error
}

func storeKey(baseURL, orgName, appName, clientID string) string {
	sum := sha256.Sum256([]byte(baseURL + "\n" + orgName + "\n" + appName + "\n" + clientID))
	return "drim:token:" + hex.EncodeToString(sum[:])[:16]
}

// get 返回当前可用的令牌，需要时换取（单飞）。ctx 结束时不再等待（换取本身继续，结果留给之后的请求）。
func (m *tokenManager) get(ctx context.Context) (string, *Error) {
	m.mu.Lock()
	if err := m.unavailableLocked(); err != nil {
		m.mu.Unlock()
		return "", err
	}
	if m.token == "" && m.store != nil && m.flight == nil {
		m.mu.Unlock()
		m.loadFromStore(ctx)
		m.mu.Lock()
		if err := m.unavailableLocked(); err != nil {
			m.mu.Unlock()
			return "", err
		}
	}
	if m.token != "" {
		remaining := time.Until(m.expiresAt)
		if remaining > blockingAhead {
			if remaining < max(m.lifetime/10, refreshMinAhead) && m.flight == nil {
				m.startLocked() // 后台换取，这个请求仍用旧令牌
			}
			token := m.token
			m.mu.Unlock()
			return token, nil
		}
	}
	f := m.flight
	if f == nil {
		f = m.startLocked()
	}
	m.mu.Unlock()
	select {
	case <-f.done:
		if f.err != nil {
			copied := *f.err
			return "", &copied
		}
		return f.token, nil
	case <-ctx.Done():
		e := ctxError(ctx)
		e.Message = "等待换取 App Token 时" + e.Message
		return "", e
	}
}

func (m *tokenManager) unavailableLocked() *Error {
	if m.closed {
		e := newError(CodeClientUnavailable, "客户端已关闭")
		e.Details = map[string]any{"cause": "closed"}
		return e
	}
	if m.permanent != nil {
		e := newError(CodeClientUnavailable, "客户端已永久失效：应用已被删除或租户已注销")
		e.Details = map[string]any{"cause": CodeUnauthenticated, "reason": m.permanent.Reason()}
		return e
	}
	if m.cooldownErr != nil && time.Now().Before(m.cooldownUntil) {
		e := newError(CodeClientUnavailable, "换取 App Token 失败（"+m.cooldownErr.Code+"）后的冷却期内，不再换取")
		e.Details = map[string]any{"cause": m.cooldownErr.Code, "until": m.cooldownUntil.Round(0)}
		if reason := m.cooldownErr.Reason(); reason != "" {
			e.Details["reason"] = reason
		}
		return e
	}
	return nil
}

func (m *tokenManager) startLocked() *tokenFlight {
	f := &tokenFlight{done: make(chan struct{})}
	m.flight = f
	go m.run(f, m.gen, m.clientID, m.clientSecret, m.storeKey)
	return f
}

func (m *tokenManager) run(f *tokenFlight, gen uint64, clientID, clientSecret, key string) {
	started := time.Now()
	token, lifetime, appID, err := m.exchange(clientID, clientSecret)
	m.mu.Lock()
	if m.flight == f {
		m.flight = nil
	}
	current := gen == m.gen
	if current {
		if err == nil {
			if m.permanent == nil {
				m.token = token
				m.expiresAt = started.Add(lifetime)
				m.lifetime = lifetime
				m.appID = appID
			}
		} else {
			m.enterCooldownLocked(err)
		}
	}
	m.mu.Unlock()
	f.token, f.err = token, err
	close(f.done)
	if err == nil {
		m.log.Info("已换取 App Token", "expires_in", lifetime)
		if current && m.store != nil {
			ctx, cancel := context.WithTimeout(m.baseCtx, storeTimeout)
			if serr := m.store.Set(ctx, key, token, started.Add(lifetime).Round(0)); serr != nil {
				m.log.Warn("令牌存储出错（Set），已忽略", "error", serr)
			}
			cancel()
		}
	}
}

func (m *tokenManager) enterCooldownLocked(err *Error) {
	var d time.Duration
	switch err.Code {
	case CodeInvalidClient, CodeIPNotAllowed:
		d = time.Minute
	case CodeTenantUnavailable, CodeAppUnavailable:
		d = 2*time.Minute + time.Duration(mathrand.Int64N(int64(3*time.Minute)))
	default:
		m.log.Error("换取 App Token 失败", "code", err.Code, "status", err.StatusCode, "request_id", err.RequestID)
		return
	}
	m.cooldownErr = err
	m.cooldownUntil = time.Now().Add(d)
	m.token = ""
	m.log.Error("换取 App Token 失败，冷却期内不再换取，请求直接返回 client_unavailable",
		"code", err.Code, "reason", err.Reason(), "cooldown", d.Round(time.Second), "request_id", err.RequestID)
}

func (m *tokenManager) loadFromStore(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	m.mu.Lock()
	key := m.storeKey
	m.mu.Unlock()
	token, expiresAt, ok, err := m.store.Get(sctx, key)
	if err != nil {
		m.log.Warn("令牌存储出错（Get），退回本进程换取", "error", err)
		return
	}
	if !ok || token == "" {
		return
	}
	remaining := time.Until(expiresAt)
	if remaining <= blockingAhead {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == "" {
		m.token = token
		m.expiresAt = time.Now().Add(remaining)
		m.lifetime = max(m.lifetime, remaining)
		m.log.Debug("从令牌存储取得 App Token", "remaining", remaining.Round(time.Second))
	}
}

// invalidate 收到不带原因的 unauthenticated：只有当前令牌仍是这个请求所用的才作废（不作废刚换来的新令牌）；
// 共享存储中同样只在令牌相同时删除。
func (m *tokenManager) invalidate(ctx context.Context, token string) {
	m.mu.Lock()
	if m.token == token {
		m.token = ""
		m.expiresAt = time.Time{}
	}
	key := m.storeKey
	m.mu.Unlock()
	if m.store != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer cancel()
		if err := m.store.CompareAndDelete(sctx, key, token); err != nil {
			m.log.Warn("令牌存储出错（CompareAndDelete），已忽略", "error", err)
		}
	}
}

// markPermanent 应用已删除或租户已注销：客户端永久失效。
func (m *tokenManager) markPermanent(e *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.permanent == nil {
		m.log.Error("应用已被删除或租户已注销，客户端永久失效；请清除保存的 Client ID 和 Client Secret", "reason", e.Reason())
	}
	m.permanent = e
	m.token = ""
}

func (m *tokenManager) updateCredentials(clientID, clientSecret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientID, m.clientSecret = clientID, clientSecret
	m.storeKey = storeKey(m.keyBase[0], m.keyBase[1], m.keyBase[2], clientID)
	m.gen++
	m.token = ""
	m.expiresAt = time.Time{}
	m.cooldownErr = nil
	m.cooldownUntil = time.Time{}
	m.flight = nil
	m.log.Info("已更新服务端凭据，下一次请求用新凭据换取 App Token")
}

func (m *tokenManager) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
}

func (m *tokenManager) currentAppID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appID
}

// exchange 换取一次令牌：网络错误、5xx 按 1、2、4 秒重试，被限流按 Retry-After 等待后重试（最多 3 次）。
func (m *tokenManager) exchange(clientID, clientSecret string) (string, time.Duration, string, *Error) {
	body := []byte(`{"grant_type":"client_credentials","client_id":`)
	body = appendString(body, clientID)
	body = append(body, `,"client_secret":`...)
	body = appendString(body, clientSecret)
	body = append(body, '}')
	retries, rateLimited := 0, 0
	for attempt := 1; ; attempt++ {
		requestID := newRequestID()
		token, lifetime, appID, e, retryAfter := m.exchangeOnce(body, requestID, attempt)
		if e == nil {
			return token, lifetime, appID, nil
		}
		e.Op = opToken
		e.Attempts = attempt
		if e.RequestID == "" {
			e.RequestID = requestID
		}
		if m.baseCtx.Err() != nil {
			closed := newError(CodeClientUnavailable, "客户端已关闭")
			closed.Details = map[string]any{"cause": "closed"}
			return "", 0, "", closed
		}
		var delay time.Duration
		switch {
		case e.Code == CodeRateLimited:
			wait := retryAfter
			if wait <= 0 {
				wait = defaultRateLimitedWait
			}
			if wait > m.maxRetryAfter || rateLimited >= 3 {
				return "", 0, "", e
			}
			rateLimited++
			delay = time.Duration(float64(wait) * (1 + 0.2*mathrand.Float64()))
		case e.temporary() && retries < len(exchangeDelays):
			delay = exchangeDelays[retries]
			retries++
		default:
			return "", 0, "", e
		}
		m.log.Warn("换取 App Token 失败，将要重试", "code", e.Code, "delay", delay)
		if sleepCtx(m.baseCtx, delay) != nil {
			return "", 0, "", e
		}
	}
}

func (m *tokenManager) exchangeOnce(body []byte, requestID string, attempt int) (string, time.Duration, string, *Error, time.Duration) {
	ctx, cancel := context.WithTimeout(m.baseCtx, m.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, nil)
	if err != nil {
		return "", 0, "", invalidParams("", "换取令牌的地址不合法："+err.Error()), 0
	}
	req.Body = &nopReadCloser{bytes.NewReader(body)}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", m.userAgent)
	req.Header.Set("X-Request-ID", requestID)
	if m.hooks.BeforeRequest != nil {
		m.hooks.BeforeRequest(ctx, req)
	}
	started := time.Now()
	resp, err := m.http.Do(req)
	var data []byte
	if err == nil {
		data, err = readLimited(resp.Body, maxTokenBody)
		resp.Body.Close()
	}
	elapsed := time.Since(started)
	info := RequestInfo{Op: opToken, Method: http.MethodPost, URL: m.tokenURL, Attempt: attempt, RequestID: requestID, Duration: elapsed}
	if err != nil {
		var e *Error
		if errors.Is(err, errTooLarge) {
			e = newError(CodeInvalidResponse, "换取令牌的响应体过大")
		} else {
			e = classify(m.baseCtx, ctx, err, true).err
			e.Message = "换取 App Token 失败：" + e.Message
		}
		info.Code = e.Code
		m.afterHook(ctx, info)
		return "", 0, "", e, 0
	}
	info.StatusCode = resp.StatusCode
	if id := resp.Header.Get("X-Request-ID"); id != "" {
		info.RequestID = id
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := errorFromResponse(resp.StatusCode, resp.Header, data, info.RequestID)
		info.Code, info.Reason = e.Code, e.Reason()
		m.afterHook(ctx, info)
		return "", 0, "", e, e.RetryAfter
	}
	m.afterHook(ctx, info)
	var parsed struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Application string          `json:"application"`
	}
	var seconds int64
	if json.Unmarshal(data, &parsed) != nil || parsed.AccessToken == "" || json.Unmarshal(parsed.ExpiresIn, &seconds) != nil || seconds <= 0 {
		e := newError(CodeInvalidResponse, "换取 App Token 的响应不合规")
		e.StatusCode = resp.StatusCode
		e.RequestID = info.RequestID
		return "", 0, "", e, 0
	}
	return parsed.AccessToken, time.Duration(seconds) * time.Second, parsed.Application, nil, 0
}

func (m *tokenManager) afterHook(ctx context.Context, info RequestInfo) {
	if m.hooks.AfterResponse != nil {
		m.hooks.AfterResponse(ctx, info)
	}
}

// nopReadCloser 请求体不设置 GetBody（net/http 不自行重发）。
type nopReadCloser struct{ *bytes.Reader }

func (nopReadCloser) Close() error { return nil }
