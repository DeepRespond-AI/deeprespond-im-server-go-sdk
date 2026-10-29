package drim

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/endpoints"
)

func discardLogger() *slog.Logger { return slog.New(discardHandler{}) }

func endpointFor(group string) *endpoints.Endpoint {
	return &endpoints.Endpoint{Name: "x", RateGroup: group}
}

func TestInvalidateKeepsNewerToken(t *testing.T) { // S-07
	m := &tokenManager{token: "new", expiresAt: time.Now().Add(time.Hour), lifetime: time.Hour, log: discardLogger()}
	m.baseCtx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	m.invalidate(context.Background(), "old")
	if m.token != "new" {
		t.Fatal("持旧令牌的请求失败不应作废新令牌")
	}
	m.invalidate(context.Background(), "new")
	if m.token != "" {
		t.Fatal("当前令牌失效时作废")
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"2":                             2 * time.Second,
		"1.5":                           1500 * time.Millisecond,
		"":                              0,
		"-3":                            0,
		"abc":                           0,
		"Tue, 06 Oct 2026 00:00:10 GMT": 10 * time.Second,
	}
	for value, want := range cases {
		if got := parseRetryAfter(value, now); got != want {
			t.Errorf("%q：%v，期望 %v", value, got, want)
		}
	}
}

func TestAppendString(t *testing.T) {
	cases := map[string]string{
		`<a href="x">&`:      `"<a href=\"x\">&"`,
		"中文 ":                "\"中文 \"",
		"tab\tline\nq\\":     `"tab\tline\nq\\"`,
		"\x01\x7f":           `"\u0001` + "\x7f\"",
		string([]byte{0xff}): `"�"`,
	}
	for in, want := range cases {
		if got := string(appendString(nil, in)); got != want {
			t.Errorf("%q：%s，期望 %s", in, got, want)
		}
	}
}

func TestEscapeSegment(t *testing.T) {
	if got := escapeSegment("a b/c+d%é"); got != "a%20b%2Fc%2Bd%25%C3%A9" {
		t.Fatal(got)
	}
}

func TestRateScope(t *testing.T) {
	tr := &transport{}
	cases := []struct {
		reason, group, want string
	}{
		{"", "", scopeClient},
		{"", "messages", scopeClient},
		{"app_message_rate", "messages", "messages"},
		{"request_rate", "reports", "reports"},
		{"group_send_rate", "messages", ""},
		{"image_busy", "", ""},
		{"app_message_rate", "", ""},
	}
	for _, c := range cases {
		ep := endpointFor(c.group)
		if got := tr.rateScope(c.reason, ep); got != c.want {
			t.Errorf("%s/%s：%q，期望 %q", c.reason, c.group, got, c.want)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(&RateLimit{PerSecond: 10, Burst: 2})
	if l.reserve(1) != 0 || l.reserve(1) != 0 {
		t.Fatal("桶的容量内不等待")
	}
	if wait := l.reserve(1); wait < 90*time.Millisecond || wait > 110*time.Millisecond {
		t.Fatal(wait)
	}
	// 批量按项数计，最多扣到桶的容量
	l2 := newRateLimiter(&RateLimit{PerSecond: 10})
	if wait := l2.reserve(100); wait > 0 {
		t.Fatal(wait)
	}
	if wait := l2.reserve(100); wait < 900*time.Millisecond || wait > 1100*time.Millisecond {
		t.Fatal(wait)
	}
}
