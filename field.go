package drim

import (
	"bytes"
	"encoding/json"
)

// Field 是修改类参数中区分“不传”“传 null”“传值”三种状态的字段（《1.功能概述文档》第 4 节）。
//
// 零值为未设置，不出现在请求中；Null 发送 null；Set 发送值。例如修改用户资料时
// Attributes: Set(map[string]Field[string]{"vip": Set("1"), "level": Null[string]()})
// 修改 vip、删除 level，Attributes: Null[map[string]Field[string]]() 清空全部。
type Field[T any] struct {
	state fieldState
	value T
}

type fieldState uint8

const (
	fieldUnset fieldState = iota
	fieldNull
	fieldValue
)

// Set 返回传值的 Field。
func Set[T any](v T) Field[T] { return Field[T]{state: fieldValue, value: v} }

// Null 返回传 null 的 Field。
func Null[T any]() Field[T] { return Field[T]{state: fieldNull} }

// IsSet 为真表示会出现在请求中（传 null 或传值）。
func (f Field[T]) IsSet() bool { return f.state != fieldUnset }

// IsNull 为真表示传 null。
func (f Field[T]) IsNull() bool { return f.state == fieldNull }

// Value 返回值；ok 为假表示未设置或为 null。
func (f Field[T]) Value() (v T, ok bool) { return f.value, f.state == fieldValue }

// String 返回 s 的指针，用于只有“不传”和“传值”两种状态的可选参数（如 UpdateUserParams.Nickname）。
func String(s string) *string { return &s }

// Int 返回 n 的指针。
func Int(n int) *int { return &n }

// Int64 返回 n 的指针（seq、版本号、时长等）。
func Int64(n int64) *int64 { return &n }

// Bool 返回 b 的指针。
func Bool(b bool) *bool { return &b }

// Float64 返回 f 的指针。
func Float64(f float64) *float64 { return &f }

// JSON 把 JSON 文本转为 json.RawMessage，用于消息的 Body、Ext 等自由内容；发送时原样保留写法（只去掉空白）。
func JSON(s string) json.RawMessage { return json.RawMessage(s) }

// Marshal 把 v 编码为 JSON，不转义 HTML 字符（<、>、& 原样），用于构造消息的 Body、Ext。
// encoding/json 的 json.Marshal 会把它们写成 < 等，消息的大小变大，租户对内容的签名也对不上。
func Marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
