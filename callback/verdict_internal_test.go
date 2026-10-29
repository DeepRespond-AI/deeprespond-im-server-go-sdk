package callback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

type hookCase struct {
	Name      string          `json:"name"`
	Hook      string          `json:"hook"`
	JoinedVia string          `json:"joined_via"`
	Targets   []string        `json:"request_targets"`
	Response  json.RawMessage `json:"response"`
	Valid     bool            `json:"valid"`
	SizeBytes int             `json:"size_bytes"`
}

// verdictOf 把用例中的响应转为 Verdict；Go 的 API 表达不了的（如空字符串的 reason、不是字符串的字段）返回 ok 为假。
func verdictOf(raw json.RawMessage) (Verdict, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return Verdict{}, false
	}
	var action string
	if json.Unmarshal(fields["action"], &action) != nil {
		return Verdict{}, false
	}
	str := func(name string) (string, bool) {
		value, present := fields[name]
		if !present || string(value) == "null" {
			return "", true
		}
		var s string
		if json.Unmarshal(value, &s) != nil || s == "" && name == "reason" {
			return "", false
		}
		return s, true
	}
	switch action {
	case "allow":
		return Allow(), true
	case "reject":
		reason, ok1 := str("reason")
		message, ok2 := str("message")
		return Reject(reason, message), ok1 && ok2
	case "modify":
		var ext drim.Field[json.RawMessage]
		if value, present := fields["ext"]; present {
			if string(value) == "null" {
				ext = drim.Null[json.RawMessage]()
			} else {
				ext = drim.Set(value)
			}
		}
		return Modify(fields["body"], ext), true
	case "partial":
		var items []map[string]json.RawMessage
		if value, present := fields["rejected"]; present && json.Unmarshal(value, &items) != nil {
			return Verdict{}, false
		}
		var targets []RejectedTarget
		for _, item := range items {
			var t RejectedTarget
			_ = json.Unmarshal(item["username"], &t.Username)
			_ = json.Unmarshal(item["reason"], &t.Reason)
			_ = json.Unmarshal(item["message"], &t.Message)
			if r, present := item["reason"]; present && string(r) == `""` {
				return Verdict{}, false
			}
			targets = append(targets, t)
		}
		return Partial(targets...), true
	}
	return Verdict{}, false
}

func TestHookResponseCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "hook_responses.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []hookCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	checked, skipped := 0, 0
	for _, c := range cases {
		if c.SizeBytes > 0 {
			skipped++ // 用 Modify 的大 body 另外测
			continue
		}
		v, ok := verdictOf(c.Response)
		if !ok {
			skipped++ // Go 的 API 表达不了，不会出现
			continue
		}
		joinedVia := c.JoinedVia
		_, err := encodeVerdict(v, c.Hook, joinedVia, c.Targets)
		if (err == nil) != c.Valid {
			t.Errorf("%s：期望 valid=%v，得到 %v", c.Name, c.Valid, err)
		}
		checked++
	}
	if checked < 70 {
		t.Fatalf("检查了 %d 个，跳过 %d 个", checked, skipped)
	}
}

func TestVerdictSize(t *testing.T) {
	// {"action":"modify","body":{"t":"..."}} 正好 64 KB 与多 1 字节
	prefix := `{"action":"modify","body":{"t":"`
	suffix := `"}}`
	fill := MaxResponseBytes - len(prefix) - len(suffix)
	ok := Modify(json.RawMessage(`{"t":"`+strings.Repeat("a", fill)+`"}`), drim.Field[json.RawMessage]{})
	if out, err := encodeVerdict(ok, "message.before_send", "", nil); err != nil || len(out) != MaxResponseBytes {
		t.Fatal(len(out), err)
	}
	tooBig := Modify(json.RawMessage(`{"t":"`+strings.Repeat("a", fill+1)+`"}`), drim.Field[json.RawMessage]{})
	if _, err := encodeVerdict(tooBig, "message.before_send", "", nil); err == nil {
		t.Fatal("超过 64 KB 应不合规")
	}
}
