package drim

import (
	"context"
	"errors"
)

// Decision 是 ProcessPending 的处理函数作出的结论（《内容安全/3.接口设计》第 4 节）。
type Decision struct {
	Decision       string                  // violation、no_violation 等（必填）
	Actions        []ModerationActionInput // 处置，只用于 violation
	CountViolation *bool                   // 是否计入作者的违规，默认是
	Note           string                  // 说明，最长 256 个字符
}

// ProcessOptions 是 ProcessPending 的选项。
type ProcessOptions struct {
	Scene       string // 只处理这个场景的记录，空为全部
	Limit       int    // 每页的条数，0 为服务端的默认值
	StopOnError bool   // 处理函数或作出结论出错时停止并返回错误；默认记警告日志后继续下一条
}

// ProcessPending 遍历租户队列中待处理的审核记录（pending、auto_violation）：读取内容，调用 handle，按返回的结论作出结论；
// handle 返回 nil 的结论为跳过。作出结论时带上读到的 Version，所以可以安全重试；记录已被别人处理（version_conflict）的跳过。
// 返回作出结论的条数；遍历本身出错（如取下一页失败）时返回已作出的条数和这个错误。
func (s *ModerationService) ProcessPending(ctx context.Context, opts ProcessOptions,
	handle func(ctx context.Context, item ModerationItem, content ItemContent) (*Decision, error)) (int, error) {
	params := ListItemsParams{Status: []string{"pending", "auto_violation"}, Scene: opts.Scene}
	params.Limit = opts.Limit
	decided := 0
	for item, err := range s.AllItems(ctx, params) {
		if err != nil {
			return decided, err
		}
		if err := s.processOne(ctx, item, handle); err != nil {
			if IsCode(err, CodeVersionConflict) {
				s.t.log.Debug("审核记录已被处理，跳过", "item_id", item.ItemID)
				continue
			}
			if errors.Is(err, errSkipped) {
				continue
			}
			if opts.StopOnError || ctx.Err() != nil {
				return decided, err
			}
			s.t.log.Warn("处理审核记录失败，继续下一条", "item_id", item.ItemID, "error", err)
			continue
		}
		decided++
	}
	return decided, nil
}

var errSkipped = errors.New("skipped")

func (s *ModerationService) processOne(ctx context.Context, item ModerationItem,
	handle func(ctx context.Context, item ModerationItem, content ItemContent) (*Decision, error)) error {
	content, err := s.ItemContent(ctx, item.ItemID)
	if err != nil {
		return err
	}
	decision, err := handle(ctx, item, *content)
	if err != nil {
		return err
	}
	if decision == nil {
		return errSkipped
	}
	version := item.Version
	_, err = s.Decide(ctx, item.ItemID, DecideParams{
		Decision:       decision.Decision,
		Actions:        decision.Actions,
		CountViolation: decision.CountViolation,
		Note:           decision.Note,
		Version:        &version,
	})
	return err
}
