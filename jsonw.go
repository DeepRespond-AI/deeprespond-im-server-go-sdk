package drim

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// jw 是请求体的编码器：按字段顺序写出紧凑的 JSON，不转义 HTML 字符和非 ASCII 字符（《3.实现与测试》第 3 节）。
// 本地检查不通过时记下第一个错误（invalid_params），由 transport.do 返回。
type jw struct {
	buf    []byte
	frames []jwFrame
	err    *Error
}

type jwFrame struct {
	object bool
	n      int    // 已写出的项数
	key    string // 对象中当前的键
}

func (w *jw) prefix() {
	if len(w.frames) == 0 {
		return
	}
	f := &w.frames[len(w.frames)-1]
	if f.object {
		return // 键已写出逗号和冒号
	}
	if f.n > 0 {
		w.buf = append(w.buf, ',')
	}
	f.n++
}

func (w *jw) key(k string) {
	f := &w.frames[len(w.frames)-1]
	if f.n > 0 {
		w.buf = append(w.buf, ',')
	}
	f.n++
	f.key = k
	w.buf = appendString(w.buf, k)
	w.buf = append(w.buf, ':')
}

func (w *jw) beginObject() {
	w.prefix()
	w.buf = append(w.buf, '{')
	w.frames = append(w.frames, jwFrame{object: true})
}

func (w *jw) endObject() {
	w.frames = w.frames[:len(w.frames)-1]
	w.buf = append(w.buf, '}')
}

func (w *jw) beginArray() {
	w.prefix()
	w.buf = append(w.buf, '[')
	w.frames = append(w.frames, jwFrame{})
}

func (w *jw) endArray() {
	w.frames = w.frames[:len(w.frames)-1]
	w.buf = append(w.buf, ']')
}

func (w *jw) str(s string) {
	w.prefix()
	w.buf = appendString(w.buf, s)
}

func (w *jw) null() {
	w.prefix()
	w.buf = append(w.buf, "null"...)
}

// path 是当前位置的参数路径（如 users[2].username），用于本地检查的错误。
func (w *jw) path(field string) string {
	var b strings.Builder
	add := func(f jwFrame, started bool) {
		if f.object {
			if f.key == "" {
				return
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(f.key)
			return
		}
		index := f.n
		if started {
			index--
		}
		b.WriteString("[" + strconv.Itoa(index) + "]")
	}
	for i := 1; i < len(w.frames); i++ {
		add(w.frames[i-1], true)
	}
	if len(w.frames) > 0 {
		add(w.frames[len(w.frames)-1], false)
	}
	if field != "" {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(field)
	}
	return b.String()
}

func (w *jw) fail(field, message string) {
	if w.err == nil {
		w.err = invalidParams(w.path(field), message)
	}
}

func (w *jw) require(ok bool, field string) {
	if !ok {
		w.fail(field, "缺少必填参数 "+w.path(field))
	}
}

func (w *jw) maxItems(field string, n, max int) {
	if n > max {
		w.fail(field, w.path(field)+" 最多 "+strconv.Itoa(max)+" 项，给了 "+strconv.Itoa(n)+" 项")
	}
}

// ---- 值的编码函数（func(*jw, T)），生成的代码组合使用 ----

func encStr(w *jw, s string) { w.str(s) }

func encInt64(w *jw, n int64) {
	w.prefix()
	w.buf = strconv.AppendInt(w.buf, n, 10)
}

func encInt(w *jw, n int) { encInt64(w, int64(n)) }

func encBool(w *jw, b bool) {
	w.prefix()
	w.buf = strconv.AppendBool(w.buf, b)
}

func encFloat(w *jw, f float64) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		w.fail("", "数值不能是 NaN 或无穷大")
		w.null()
		return
	}
	text, _ := json.Marshal(f)
	w.prefix()
	w.buf = append(w.buf, text...)
}

func encTime(w *jw, t time.Time) { w.str(formatTime(t)) }

// encObject 写出自由内容（消息的 body、ext 等）：必须是合法的 JSON 对象；去掉空白，其余写法（转义、1.0 这样的数字）原样保留。
func encObject(w *jw, raw json.RawMessage) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		w.fail("", "必须是 JSON 对象")
		w.null()
		return
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		w.fail("", "不是合法的 JSON："+err.Error())
		w.null()
		return
	}
	w.prefix()
	w.buf = append(w.buf, buf.Bytes()...)
}

func encSlice[E any](enc func(*jw, E)) func(*jw, []E) {
	return func(w *jw, items []E) {
		w.beginArray()
		for _, item := range items {
			enc(w, item)
		}
		w.endArray()
	}
}

func encMap[E any](enc func(*jw, E)) func(*jw, map[string]E) {
	return func(w *jw, m map[string]E) {
		w.beginObject()
		for _, k := range sortedKeys(m) {
			w.key(k)
			enc(w, m[k])
		}
		w.endObject()
	}
}

// encFieldMap 写出值可以为 null 的键值对（如修改资料的 attributes）：未设置的项跳过，Null 的项写 null。
func encFieldMap[E any](enc func(*jw, E)) func(*jw, map[string]Field[E]) {
	return func(w *jw, m map[string]Field[E]) {
		w.beginObject()
		for _, k := range sortedKeys(m) {
			f := m[k]
			if !f.IsSet() {
				continue
			}
			w.key(k)
			if f.IsNull() {
				w.null()
				continue
			}
			enc(w, f.value)
		}
		w.endObject()
	}
}

func writeField[T any](w *jw, key string, f Field[T], enc func(*jw, T)) {
	if !f.IsSet() {
		return
	}
	w.key(key)
	if f.IsNull() {
		w.null()
		return
	}
	enc(w, f.value)
}

func sortedKeys[E any](m map[string]E) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// formatTime 请求中的时间：RFC 3339 的 UTC。
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// appendString 写出 JSON 字符串：只转义引号、反斜杠和控制字符，不转义 <、>、& 和非 ASCII 字符；
// 不合法的 UTF-8 写成 �（与 encoding/json 相同）。
func appendString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `�`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// ---- 响应 ----

// cloneRaw 复制原始 JSON（json.Unmarshal 传给 UnmarshalJSON 的切片不能保留）。
func cloneRaw(b []byte) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// nullRaw 把 JSON 的 null 转为 nil（自由内容为 null 时调用方看到 nil）。
func nullRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 || string(bytes.TrimSpace(r)) == "null" {
		return nil
	}
	return r
}

func countSet(values ...*int64) int {
	n := 0
	for _, v := range values {
		if v != nil {
			n++
		}
	}
	return n
}
