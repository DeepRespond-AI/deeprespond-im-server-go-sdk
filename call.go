package drim

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/endpoints"
)

// call 是一次调用编码好的请求：路径、查询参数、请求体，以及本地检查的结果。生成的方法构造它后交给 transport.do。
type call struct {
	ep       *endpoints.Endpoint
	urlPath  string
	rawQuery []byte
	w        *jw
	unsafe   bool // 按参数变为不幂等（x-drim-unsafe-if）
	items    int  // 批量接口的项数（本地限速按项数计）
	err      *Error
	after    func(out any, attempts int) // 解码之后由手写的函数整理结果
}

func (t *transport) newCall(ep *endpoints.Endpoint) *call {
	return &call{ep: ep, urlPath: ep.Path}
}

// path 填入路径参数（按 URL 规则编码）；空字符串和 . .. 在本地拒绝。
func (c *call) path(name, value string) {
	switch value {
	case "":
		c.fail(name, "缺少路径参数 "+name)
		return
	case ".", "..":
		c.fail(name, "路径参数 "+name+" 不能是 . 或 ..")
		return
	}
	c.urlPath = strings.Replace(c.urlPath, "{"+name+"}", escapeSegment(value), 1)
}

func (c *call) query(key, value string) {
	if len(c.rawQuery) > 0 {
		c.rawQuery = append(c.rawQuery, '&')
	}
	c.rawQuery = append(c.rawQuery, url.QueryEscape(key)...)
	c.rawQuery = append(c.rawQuery, '=')
	c.rawQuery = append(c.rawQuery, url.QueryEscape(value)...)
}

func (c *call) body() *jw {
	c.w = &jw{}
	return c.w
}

func (c *call) fail(field, message string) {
	if c.err == nil {
		c.err = invalidParams(field, message)
	}
}

func (c *call) require(ok bool, field string) {
	if !ok {
		c.fail(field, "缺少必填参数 "+field)
	}
}

func (c *call) maxItems(field string, n, max int) {
	if n > max {
		c.fail(field, field+" 最多 "+strconv.Itoa(max)+" 项，给了 "+strconv.Itoa(n)+" 项")
	}
}

// checkClientMsgID 调用方给出的 client_msg_id：1 到 64 个可见 ASCII 字符，不以 evt_、sys_ 开头（《2.接口设计》第 7.4 节）。
func (c *call) checkClientMsgID(id string) {
	if id == "" {
		return
	}
	if len(id) > 64 {
		c.fail("client_msg_id", "client_msg_id 最长 64 个字符")
		return
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			c.fail("client_msg_id", "client_msg_id 只能是可见的 ASCII 字符（不含空格）")
			return
		}
	}
	if strings.HasPrefix(id, "evt_") || strings.HasPrefix(id, "sys_") {
		c.fail("client_msg_id", "client_msg_id 不能以 evt_、sys_ 开头（服务端保留）")
	}
}

// escapeSegment 编码路径中的一段：除 A-Z a-z 0-9 - _ . ~ 外都编码。
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0xF])
	}
	return b.String()
}
