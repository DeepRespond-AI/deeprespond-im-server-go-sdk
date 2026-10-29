package drim

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Client 是一个应用的服务端 SDK 客户端（《2.接口设计》第 1、3 节）。
//
// 客户端可以被多个协程同时使用，内部共享 App Token、连接池和限流等待，应在进程中复用，不要每个请求新建；
// 同一进程管理多个应用时各建一个客户端。
type Client struct {
	Users         *UsersService
	Friends       *FriendsService
	Blacklist     *BlacklistService
	Groups        *GroupsService
	Messages      *MessagesService
	Conversations *ConversationsService
	Media         *MediaService
	Push          *PushService
	Chatrooms     *ChatroomsService
	Moderation    *ModerationService
	Calls         *CallsService
	Channels      *ChannelsService
	Presence      *PresenceService

	t *transport
}

// New 检查配置并创建客户端，不发请求（第一次调用时换取 App Token）。
func New(cfg Config) (*Client, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	for name, value := range map[string]string{
		"OrgName": cfg.OrgName, "AppName": cfg.AppName, "ClientID": cfg.ClientID, "ClientSecret": cfg.ClientSecret,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("drim: " + name + " 不能为空")
		}
	}
	if cfg.Timeout < 0 || cfg.MaxRetryAfter < 0 {
		return nil, errors.New("drim: 时长不能为负数")
	}
	if cfg.Retry.MaxAttempts < 0 || cfg.Retry.MaxRateLimited < 0 {
		return nil, errors.New("drim: 重试次数不能为负数")
	}
	if cfg.RateLimit != nil && (cfg.RateLimit.PerSecond <= 0 || cfg.RateLimit.Burst < 0) {
		return nil, errors.New("drim: RateLimit.PerSecond 必须大于 0，Burst 不能为负数")
	}
	httpClient := cfg.HTTPClient
	owns := false
	if httpClient == nil {
		httpClient = defaultHTTPClient()
		owns = true
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	userAgent := "deeprespond-im-server-sdk/go/" + Version
	if s := strings.TrimSpace(cfg.UserAgentSuffix); s != "" {
		userAgent += " " + s
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxAttempts := cfg.Retry.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultMaxAttempts
	}
	maxRateLimited := cfg.Retry.MaxRateLimited
	if maxRateLimited == 0 {
		maxRateLimited = defaultMaxRateLimited
	}
	if cfg.Retry.Disabled {
		maxAttempts, maxRateLimited = 1, 0
	}
	maxRetryAfter := cfg.MaxRetryAfter
	if maxRetryAfter == 0 {
		maxRetryAfter = defaultMaxRetryAfter
	}
	prefix := base + "/" + escapeSegment(cfg.OrgName) + "/" + escapeSegment(cfg.AppName)
	baseCtx, cancel := context.WithCancel(context.Background())
	tokens := &tokenManager{
		clientID:      cfg.ClientID,
		clientSecret:  cfg.ClientSecret,
		tokenURL:      prefix + "/token",
		keyBase:       [3]string{base, cfg.OrgName, cfg.AppName},
		storeKey:      storeKey(base, cfg.OrgName, cfg.AppName, cfg.ClientID),
		store:         cfg.TokenStore,
		http:          httpClient,
		userAgent:     userAgent,
		timeout:       timeout,
		maxRetryAfter: maxRetryAfter,
		hooks:         cfg.Hooks,
		log:           logger.With("component", "drim.token"),
		baseCtx:       baseCtx,
		cancel:        cancel,
	}
	t := &transport{
		prefix:         prefix,
		http:           httpClient,
		ownsHTTP:       owns,
		timeout:        timeout,
		maxAttempts:    maxAttempts,
		maxRateLimited: maxRateLimited,
		maxRetryAfter:  maxRetryAfter,
		tokens:         tokens,
		hooks:          cfg.Hooks,
		log:            logger.With("component", "drim"),
		userAgent:      userAgent,
	}
	if cfg.RateLimit != nil {
		t.limiter = newRateLimiter(cfg.RateLimit)
	}
	return &Client{
		Users:         &UsersService{t: t},
		Friends:       &FriendsService{t: t},
		Blacklist:     &BlacklistService{t: t},
		Groups:        &GroupsService{t: t},
		Messages:      &MessagesService{t: t},
		Conversations: &ConversationsService{t: t},
		Media:         &MediaService{t: t},
		Push:          &PushService{t: t},
		Chatrooms:     &ChatroomsService{t: t},
		Moderation:    &ModerationService{t: t},
		Calls:         &CallsService{t: t},
		Channels:      &ChannelsService{t: t},
		Presence:      &PresenceService{t: t},
		t:             t,
	}, nil
}

// UpdateCredentials 在控制台轮换 Secret 后更新凭据：丢弃当前令牌、结束换取失败后的冷却期，下一次请求用新凭据换取。
// 令牌存储的键随 Client ID 变化。
func (c *Client) UpdateCredentials(clientID, clientSecret string) {
	c.t.tokens.updateCredentials(clientID, clientSecret)
}

// Token 返回当前有效的 App Token（排查用，需要时换取）。不要把它写入日志或交给客户端。
func (c *Client) Token(ctx context.Context) (string, error) {
	token, err := c.t.tokens.get(ctx)
	if err != nil {
		if err.Op == "" {
			err.Op = opToken
		}
		return "", err
	}
	return token, nil
}

// AppID 换取令牌时得到的应用 ID，还没换取过时为空。
func (c *Client) AppID() string { return c.t.tokens.currentAppID() }

// Stats 按接口名统计的请求数、失败数、重试数和限流等待的总时长（快照）。
func (c *Client) Stats() Stats { return c.t.stats.snapshot() }

// Close 停止后台的换取，关闭 SDK 自己创建的 HTTP 客户端的空闲连接；之后的调用返回 client_unavailable。
func (c *Client) Close() error {
	if c.t.closed.Swap(true) {
		return nil
	}
	c.t.tokens.close()
	if c.t.ownsHTTP {
		if tr, ok := c.t.http.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	return nil
}
