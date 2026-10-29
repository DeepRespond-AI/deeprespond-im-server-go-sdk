package callback

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// 随机数和事件去重的存储（《1.功能概述文档》第 11.2、11.3 节）。内存存储只在一个进程内有效：
// 多个实例、多台服务器部署时换成共享存储（如 Redis，见 README 中的示例）。

// ClaimState 是占用事件 ID 的结果。
type ClaimState int

const (
	Acquired ClaimState = iota + 1 // 占用成功，可以处理
	Done                           // 已经处理过：跳过，算作成功
	Busy                           // 正在另一个请求中处理：这一批返回 503，服务端稍后重发
)

// NonceStore 记录 10 分钟内见过的随机数。共享存储的写法如 Redis 的 SET NX PX 600000。
type NonceStore interface {
	// Seen 已存在返回 true，否则记下（ttl 后过期）并返回 false。
	Seen(ctx context.Context, nonce string, ttl time.Duration) (bool, error)
}

// DedupStore 按事件 ID 去重，“占用、完成、释放”三步（《2.接口设计》第 9 节）：开始处理时占用（带处理时限），
// 处理成功后改为已完成，失败时释放。占用返回的令牌用于完成和释放，只有令牌相同才生效：占用过期后被别的请求重新占用时，
// 原来的请求不会把别人的占用改掉。
type DedupStore interface {
	Claim(ctx context.Context, eventID string, ttl time.Duration) (state ClaimState, token string, err error)
	Complete(ctx context.Context, eventID, token string) error
	Release(ctx context.Context, eventID, token string) error
}

// ErrStoreFull 内存随机数存储已满（不淘汰未到期的随机数，淘汰了就能重放）：这个请求返回 503。
var ErrStoreFull = errors.New("callback: 内存随机数存储已满")

// expiring 是一把锁保护的字典和按到期时间排序的最小堆；写入时顺带清理已到期的。
type expiring struct {
	mu         sync.Mutex
	maxEntries int
	items      map[string]entry
	heap       expiryHeap
	now        func() time.Time
}

type entry struct {
	expires time.Time
	value   string
}

type heapItem struct {
	expires time.Time
	key     string
}

type expiryHeap []heapItem

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)        { *h = append(*h, x.(heapItem)) }
func (h *expiryHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

func newExpiring(maxEntries int) *expiring {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &expiring{maxEntries: maxEntries, items: map[string]entry{}, now: time.Now}
}

func (s *expiring) purge(now time.Time) {
	for s.heap.Len() > 0 && !s.heap[0].expires.After(now) {
		item := heap.Pop(&s.heap).(heapItem)
		if current, ok := s.items[item.key]; ok && current.expires.Equal(item.expires) {
			delete(s.items, item.key)
		}
	}
}

func (s *expiring) put(key, value string, expires time.Time) {
	s.items[key] = entry{expires, value}
	heap.Push(&s.heap, heapItem{expires, key})
	if s.heap.Len() > 4*s.maxEntries {
		// 同一个键多次写入留下的旧记录：重建堆
		s.heap = s.heap[:0]
		for k, e := range s.items {
			s.heap = append(s.heap, heapItem{e.expires, k})
		}
		heap.Init(&s.heap)
	}
}

func (s *expiring) get(key string, now time.Time) (string, bool) {
	current, ok := s.items[key]
	if !ok || !current.expires.After(now) {
		return "", false
	}
	return current.value, true
}

func (s *expiring) evictOldest() {
	for s.heap.Len() > 0 {
		item := heap.Pop(&s.heap).(heapItem)
		if current, ok := s.items[item.key]; ok && current.expires.Equal(item.expires) {
			delete(s.items, item.key)
			return
		}
	}
}

// MemoryNonceStore 是进程内存中的随机数存储，默认最多 100 万个；已满时返回 ErrStoreFull，不淘汰未到期的。
type MemoryNonceStore struct{ s *expiring }

// NewMemoryNonceStore 创建内存随机数存储。
func NewMemoryNonceStore() *MemoryNonceStore { return &MemoryNonceStore{newExpiring(1_000_000)} }

// Seen 实现 NonceStore。
func (m *MemoryNonceStore) Seen(_ context.Context, nonce string, ttl time.Duration) (bool, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purge(now)
	if _, ok := s.get(nonce, now); ok {
		return true, nil
	}
	if len(s.items) >= s.maxEntries {
		return false, ErrStoreFull
	}
	s.put(nonce, "1", now.Add(ttl))
	return false, nil
}

// MemoryDedupStore 是进程内存中的事件去重存储：处理完成后保留 retention（默认 24 小时），默认最多 100 万个；
// 超出上限时淘汰最早的并记警告日志（被淘汰的事件再次到达会被重复处理）。
type MemoryDedupStore struct {
	s         *expiring
	retention time.Duration
	warnedAt  time.Time
	log       *slog.Logger
}

// NewMemoryDedupStore 创建内存去重存储；retention 为处理完成后保留的时长（0 为 24 小时）。
// 需要重新投递几天前的事件时，用共享存储保留 7 天（失败的投递在控制台保留 7 天）。
func NewMemoryDedupStore(retention time.Duration) *MemoryDedupStore {
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	return &MemoryDedupStore{s: newExpiring(1_000_000), retention: retention, log: slog.Default()}
}

// Claim 实现 DedupStore。
func (m *MemoryDedupStore) Claim(_ context.Context, eventID string, ttl time.Duration) (ClaimState, string, error) {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purge(now)
	if value, ok := s.get(eventID, now); ok {
		if value == "done" {
			return Done, "", nil
		}
		return Busy, "", nil
	}
	for len(s.items) >= s.maxEntries {
		s.evictOldest()
		if now.Sub(m.warnedAt) > time.Minute {
			m.warnedAt = now
			m.log.Warn("内存去重存储已满，淘汰了最早的记录；被淘汰的事件再次到达会被重复处理，请换成共享存储")
		}
	}
	token := newToken()
	s.put(eventID, "busy:"+token, now.Add(ttl))
	return Acquired, token, nil
}

// Complete 实现 DedupStore：只在占用的令牌相同时改为已完成。
func (m *MemoryDedupStore) Complete(_ context.Context, eventID, token string) error {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if value, ok := s.get(eventID, now); ok && value == "busy:"+token {
		s.put(eventID, "done", now.Add(m.retention))
	}
	return nil
}

// Release 实现 DedupStore：只在占用的令牌相同时释放。
func (m *MemoryDedupStore) Release(_ context.Context, eventID, token string) error {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.items[eventID]; ok && current.value == "busy:"+token {
		delete(s.items, eventID)
	}
	return nil
}

func newToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
