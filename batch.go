package drim

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

// ItemError 是批量接口中一项的失败（《2.接口设计》第 6 节）。不是 *Error：请求本身成功了，只有这一项失败。
type ItemError struct {
	Key     string         `json:"-"`       // 项的标识：用户名、消息 ID、记录 ID、属性的键等（服务端返回的，用户名为小写）
	Code    string         `json:"code"`    // 错误码，与 *Error 的相同
	Message string         `json:"message"` // 说明，只用于日志；有的接口没有
	Details map[string]any `json:"details"` // 可能为 nil
}

// Reason 返回 details.reason，没有时为空。
func (e ItemError) Reason() string {
	s, _ := e.Details["reason"].(string)
	return s
}

// Error 让 ItemError 也可以作为 error 使用。
func (e ItemError) Error() string {
	text := e.Code
	if e.Key != "" {
		text = e.Key + ": " + text
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

func sortedItemErrors(m map[string]ItemError) []ItemError {
	if len(m) == 0 {
		return nil
	}
	out := make([]ItemError, 0, len(m))
	for k, v := range m {
		v.Key = k
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func batchErr(failed []ItemError) error {
	if len(failed) == 0 {
		return nil
	}
	return &BatchError{Failed: failed}
}

// batchAll 按上限拆分、依次提交、合并结果；服务端以 too_many_items 给出更小的上限（应用的设置）时按它重新拆分。
// 某一批整体失败时停止，返回已完成部分的合并结果（没有时为 nil）和那一批的错误。
func batchAll[E any, R any](ctx context.Context, items []E, size int, send func(context.Context, []E) (*R, error),
	merge func([]*R) *R) (*R, error) {
	if len(items) == 0 {
		return send(ctx, items) // 由本地检查给出 invalid_params
	}
	var results []*R
	for len(items) > 0 {
		n := min(size, len(items))
		result, err := send(ctx, items[:n])
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Reason() == "too_many_items" {
				if limit := detailInt(e.Details, "max"); limit > 0 && limit < n {
					size = limit
					continue
				}
			}
			if len(results) == 0 {
				return nil, err
			}
			return merge(results), err
		}
		results = append(results, result)
		items = items[n:]
	}
	return merge(results), nil
}

func detailInt(details map[string]any, key string) int {
	switch v := details[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// mergedRaw 合并后的结果的 Raw：{"batches":[各批的原始 JSON]}。
func mergedRaw(raws []json.RawMessage) json.RawMessage {
	out := []byte(`{"batches":[`)
	for i, raw := range raws {
		if i > 0 {
			out = append(out, ',')
		}
		if len(raw) == 0 {
			raw = json.RawMessage("null")
		}
		out = append(out, raw...)
	}
	return append(out, "]}"...)
}

func mergeBatchCreateUsers(results []*BatchCreateUsersResult) *BatchCreateUsersResult {
	if len(results) == 1 {
		return results[0]
	}
	out := &BatchCreateUsersResult{}
	var raws []json.RawMessage
	for _, r := range results {
		out.Created = append(out.Created, r.Created...)
		out.Failed = append(out.Failed, r.Failed...)
		raws = append(raws, r.Raw)
	}
	out.Raw = mergedRaw(raws)
	return out
}

func mergeBatchAddFriends(results []*BatchAddFriendsResult) *BatchAddFriendsResult {
	if len(results) == 1 {
		return results[0]
	}
	out := &BatchAddFriendsResult{}
	var raws []json.RawMessage
	for _, r := range results {
		out.Added = append(out.Added, r.Added...)
		out.Failed = append(out.Failed, r.Failed...)
		raws = append(raws, r.Raw)
	}
	out.Raw = mergedRaw(raws)
	return out
}

func mergeBatchSend(results []*BatchSendResult) *BatchSendResult {
	if len(results) == 1 {
		return results[0]
	}
	out := &BatchSendResult{}
	var raws []json.RawMessage
	for _, r := range results {
		out.Results = append(out.Results, r.Results...)
		raws = append(raws, r.Raw)
	}
	out.Raw = mergedRaw(raws)
	return out
}

// fixBatchDecide 批量结论被自动重试过时：已结束的记录单项为 version_conflict（details.current 为记录），
// 结论与这次相同的按成功处理（前一次尝试已经作出了这个结论）。
func fixBatchDecide(r *BatchDecideResult, decision string, attempts int) {
	if attempts <= 1 {
		return
	}
	for i := range r.Results {
		item := &r.Results[i]
		if item.Error == nil || item.Error.Code != CodeVersionConflict {
			continue
		}
		current, ok := item.Error.Details["current"].(map[string]any)
		if !ok {
			continue
		}
		decided, _ := current["decision"].(map[string]any)
		if result, _ := decided["result"].(string); result == "" || result != decision {
			continue
		}
		data, err := json.Marshal(current)
		if err != nil {
			continue
		}
		var parsed ModerationItem
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}
		item.Item = &parsed
		item.Error = nil
	}
}
