package drim

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
)

// ListOptions 游标分页的参数：Cursor 为上一页的 NextCursor（原样传回，不解析），Limit 为 0 时用服务端的默认值。
// 游标只在同样的筛选条件下有效，改变条件要从第一页开始。
type ListOptions struct {
	Cursor string
	Limit  int
}

// Page 是游标分页的一页。一页的条数可能少于 Limit 甚至为 0 而仍有下一页，是否结束只看 NextCursor。
type Page[T any] struct {
	Items      []T
	NextCursor string          // 空表示没有下一页（null 和空字符串都转为空）
	Raw        json.RawMessage // 这一页的原始 JSON
}

// UnmarshalJSON 解码并保存原始 JSON。
func (p *Page[T]) UnmarshalJSON(b []byte) error {
	var aux struct {
		Items      []T     `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	p.Items = aux.Items
	p.NextCursor = ""
	if aux.NextCursor != nil {
		p.NextCursor = *aux.NextCursor
	}
	p.Raw = cloneRaw(b)
	return nil
}

func (p *Page[T]) pageItems() []T     { return p.Items }
func (p *Page[T]) pageCursor() string { return p.NextCursor }

// VersionedPage 是带版本号的列表（好友、黑名单、群成员、推送的会话免打扰）的一页。
// KnownVersion 只在第一页有效：与当前版本相同时 NotModified 为真，Items 为空、NextCursor 为空。
type VersionedPage[T any] struct {
	Page[T]
	Version     int64 // friend_version、blacklist_version、member_version、settings_version
	NotModified bool
	Count       int // 好友数、黑名单人数、群成员数；推送的会话免打扰没有
	MaxCount    int // 好友列表、黑名单才有
}

// UnmarshalJSON 解码并保存原始 JSON；版本号、数量的字段名因接口而不同，由 SDK 按接口填入。
func (p *VersionedPage[T]) UnmarshalJSON(b []byte) error {
	if err := p.Page.UnmarshalJSON(b); err != nil {
		return err
	}
	var aux struct {
		NotModified bool `json:"not_modified"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	p.NotModified = aux.NotModified
	return nil
}

func (p *VersionedPage[T]) fillVersion(version, count, maxCount string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(p.Raw, &fields) != nil {
		return
	}
	read := func(name string) int64 {
		var n int64
		if name != "" {
			_ = json.Unmarshal(fields[name], &n)
		}
		return n
	}
	if version == "" {
		version = "version"
	}
	p.Version = read(version)
	p.Count = int(read(count))
	p.MaxCount = int(read(maxCount))
}

type versionFiller interface {
	fillVersion(version, count, maxCount string)
}

// SeqPage 是按序号取的会话消息（按序号从小到大）。是否结束只看 HasMore（按 AroundSeq 取时为两个方向各自的
// HasMoreBefore、HasMoreAfter）；一页可能为空而 HasMore 为真（一页最多扫描 1000 个序号或 1 MB）。
type SeqPage struct {
	Items         []Message
	HasMore       bool
	NextAfterSeq  *int64 // 下一页的 AfterSeq（向新的方向）
	NextBeforeSeq *int64 // 下一页的 BeforeSeq（向旧的方向）
	HasMoreBefore *bool  // 只在按 AroundSeq 取时有
	HasMoreAfter  *bool  // 只在按 AroundSeq 取时有
	Raw           json.RawMessage
}

// UnmarshalJSON 解码并保存原始 JSON。
func (p *SeqPage) UnmarshalJSON(b []byte) error {
	var aux struct {
		Items         []Message `json:"items"`
		HasMore       bool      `json:"has_more"`
		NextAfterSeq  *int64    `json:"next_after_seq"`
		NextBeforeSeq *int64    `json:"next_before_seq"`
		HasMoreBefore *bool     `json:"has_more_before"`
		HasMoreAfter  *bool     `json:"has_more_after"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*p = SeqPage{
		Items: aux.Items, HasMore: aux.HasMore, NextAfterSeq: aux.NextAfterSeq, NextBeforeSeq: aux.NextBeforeSeq,
		HasMoreBefore: aux.HasMoreBefore, HasMoreAfter: aux.HasMoreAfter, Raw: cloneRaw(b),
	}
	return nil
}

// ExportPage 是按消息 ID 导出的一页（从旧到新）。是否结束只看 HasMore：服务端在本页条数等于 Limit 时就给 true，下一页可能为空。
type ExportPage struct {
	Items              []Message
	NextAfterMessageID string // 下一页的 AfterMessageID
	HasMore            bool
	Raw                json.RawMessage
}

// UnmarshalJSON 解码并保存原始 JSON。
func (p *ExportPage) UnmarshalJSON(b []byte) error {
	var aux struct {
		Items              []Message `json:"items"`
		NextAfterMessageID *string   `json:"next_after_message_id"`
		HasMore            bool      `json:"has_more"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*p = ExportPage{Items: aux.Items, HasMore: aux.HasMore, Raw: cloneRaw(b)}
	if aux.NextAfterMessageID != nil {
		p.NextAfterMessageID = *aux.NextAfterMessageID
	}
	return nil
}

// iterPages 逐项遍历游标分页的列表：按 next_cursor 是否为空判断结束；出错时产出错误后结束。
func iterPages[T any](ctx context.Context, cursor string, fetch func(ctx context.Context, cursor string) ([]T, string, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for {
			items, next, err := fetch(ctx, cursor)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range items {
				if !yield(item, nil) {
					return
				}
			}
			if next == "" {
				return
			}
			if next == cursor {
				var zero T
				e := newError(CodeInvalidResponse, "服务端返回的下一页游标与这一页相同，停止遍历")
				yield(zero, e)
				return
			}
			cursor = next
		}
	}
}

// dedupe 按键去重（聊天室成员：完全离开后又进入的用户可能在后面的页再次出现）。
func dedupe[T any](key func(T) string, seq iter.Seq2[T, error]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		seen := map[string]bool{}
		for item, err := range seq {
			if err == nil {
				k := key(item)
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			if !yield(item, err) {
				return
			}
		}
	}
}

// ExportAll 逐条遍历消息导出（按消息 ID 从旧到新），自动翻页：按 has_more 判断结束（最后一页可能为空）。
// 从 params.AfterMessageID 之后开始；出错时产出这个错误后结束，调用方可以记下最后一条的消息 ID，之后从那里继续。
func (s *MessagesService) ExportAll(ctx context.Context, params ExportParams) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		p := params
		for {
			page, err := s.Export(ctx, p)
			if err != nil {
				yield(Message{}, err)
				return
			}
			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}
			if !page.HasMore || page.NextAfterMessageID == "" || page.NextAfterMessageID == p.AfterMessageID {
				return
			}
			p.AfterMessageID = page.NextAfterMessageID
		}
	}
}

// AllMessages 逐条遍历会话消息（服务端视角），自动翻页：给出 AfterSeq（或只给 StartTime）时向新的方向（序号从小到大），
// 否则从 BeforeSeq（不给为最新）向旧的方向（序号从大到小）。按 has_more 判断结束，一页可能为空。不支持 AroundSeq。
func (s *ConversationsService) AllMessages(ctx context.Context, conversationID string, params SeqParams) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		if params.AroundSeq != nil {
			yield(Message{}, invalidParams("around_seq", "AllMessages 不支持 AroundSeq，请用 Messages"))
			return
		}
		p := params
		for {
			page, err := s.Messages(ctx, conversationID, p)
			if err != nil {
				yield(Message{}, err)
				return
			}
			forward := p.AfterSeq != nil || (p.BeforeSeq == nil && page.NextAfterSeq != nil && page.NextBeforeSeq == nil)
			items := page.Items
			if !forward {
				items = slices.Clone(items)
				slices.Reverse(items)
			}
			for _, item := range items {
				if !yield(item, nil) {
					return
				}
			}
			if !page.HasMore {
				return
			}
			if forward {
				if page.NextAfterSeq == nil || (p.AfterSeq != nil && *page.NextAfterSeq == *p.AfterSeq) {
					return
				}
				next := *page.NextAfterSeq
				p.AfterSeq, p.BeforeSeq = &next, nil
			} else {
				if page.NextBeforeSeq == nil || (p.BeforeSeq != nil && *page.NextBeforeSeq == *p.BeforeSeq) {
					return
				}
				next := *page.NextBeforeSeq
				p.BeforeSeq, p.AfterSeq = &next, nil
			}
		}
	}
}
