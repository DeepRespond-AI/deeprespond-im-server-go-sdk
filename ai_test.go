package drim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/callback"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

// fakeAI 在测试服务器上模拟流的开始、按批写入和结束：按服务端的规则应用增量（append 拼接、put 设置），结束时返回累积的片段。
type fakeAI struct {
	mu     sync.Mutex
	parts  []map[string]any
	acked  int64
	closed string // 不为空时写入返回 stream_closed
}

func newFakeAI(t *testing.T, srv *drimtest.Server) *fakeAI {
	t.Helper()
	fake := &fakeAI{}
	stream := func(status string) map[string]any {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return map[string]any{"stream_id": "1841000000000000123", "bot": "assistant", "to": "zhangsan", "conversation_id": "1840098765432101",
			"message_id": "1841000000000000124", "seq": 9, "client_stream_id": nil, "reply_to_seq": nil, "status": status, "last_seq": fake.acked,
			"finish_reason": "stop", "error": nil, "model": "m", "usage": nil, "content_bytes": 0, "first_delta_ms": nil,
			"started_at": "2026-09-28T08:00:00.000Z", "finished_at": nil, "content": fake.parts}
	}
	writeJSON := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	srv.Handle("ai.create_stream", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"stream": stream("streaming")})
	})
	srv.Handle("ai.write_deltas", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Deltas []struct {
				Seq int64 `json:"seq"`
				Ops []struct {
					Op    string         `json:"op"`
					Index int            `json:"index"`
					Type  string         `json:"type"`
					Text  string         `json:"text"`
					Part  map[string]any `json:"part"`
				} `json:"ops"`
			} `json:"deltas"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("按批写入的请求体：%v", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.closed != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write(drimtest.ErrorBody("stream_closed", "回复已结束", map[string]any{"reason": fake.closed}))
			return
		}
		for _, delta := range req.Deltas {
			if delta.Seq <= fake.acked {
				continue
			}
			if delta.Seq != fake.acked+1 {
				t.Errorf("增量跳号：已确认 %d，收到 %d", fake.acked, delta.Seq)
			}
			for _, op := range delta.Ops {
				switch {
				case op.Op == "append" && op.Index == len(fake.parts):
					fake.parts = append(fake.parts, map[string]any{"type": op.Type, "text": op.Text})
				case op.Op == "append":
					fake.parts[op.Index]["text"] = fake.parts[op.Index]["text"].(string) + op.Text
				case op.Index == len(fake.parts):
					fake.parts = append(fake.parts, op.Part)
				default:
					fake.parts[op.Index] = op.Part
				}
			}
			fake.acked = delta.Seq
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"acked_seq": fake.acked})
	})
	srv.Handle("ai.finish_stream", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			LastSeq int64 `json:"last_seq"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.LastSeq != fake.acked {
			t.Errorf("结束时 last_seq = %d，已确认 %d", req.LastSeq, fake.acked)
		}
		writeJSON(w, http.StatusOK, map[string]any{"stream": stream("completed")})
	})
	srv.Handle("ai.fail_stream", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"stream": stream("failed")})
	})
	return fake
}

func (fake *fakeAI) snapshot() []map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	data, _ := json.Marshal(fake.parts)
	var parts []map[string]any
	_ = json.Unmarshal(data, &parts)
	return parts
}

func expectParts(t *testing.T, got []map[string]any, want ...string) {
	t.Helper()
	var summary []string
	for _, part := range got {
		text, _ := part["text"].(string)
		state, _ := part["state"].(string)
		summary = append(summary, part["type"].(string)+":"+text+state)
	}
	if strings.Join(summary, "|") != strings.Join(want, "|") {
		t.Fatalf("片段 = %q，期望 %q", summary, want)
	}
}

func startStream(t *testing.T, client *drim.Client) *drim.Stream {
	t.Helper()
	stream, err := client.AI.StartStream(context.Background(), drim.StartStreamInput{Bot: "assistant", To: "zhangsan", BatchOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestStreamManualWrites(t *testing.T) {
	srv := newServer(t)
	fake := newFakeAI(t, srv)
	client := newClient(t, srv)
	// 不指定 BatchOnly：测试服务器没有持续写入的接口（404），自动改用按批写入。
	stream, err := client.AI.StartStream(context.Background(), drim.StartStreamInput{Bot: "assistant", To: "zhangsan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return stream.WriteReasoning("先") },
		func() error { return stream.WriteReasoning("想") },
		func() error { return stream.WriteTool("t1", "search", "搜索", drim.ToolRunning) },
		func() error { return stream.WriteText("你") },
		func() error { return stream.WriteText("好") },
		func() error { return stream.WriteTool("t1", "search", "搜索", drim.ToolDone) },
		func() error {
			return stream.WriteImage(drim.ImagePart{URL: "https://cdn.example.com/a.png", Width: 10})
		},
		func() error { return stream.WriteText("！") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	result, err := stream.Finish(context.Background(), drim.FinishOptions{Model: "m"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("结束 = %v %v", result, err)
	}
	expectParts(t, fake.snapshot(), "reasoning:先想", "tool:done", "text:你好", "image:", "text:！")
	if err := stream.WriteText("x"); err == nil {
		t.Fatal("结束后不能再写入")
	}
}

func TestStreamClosedCancelsContext(t *testing.T) {
	srv := newServer(t)
	fake := newFakeAI(t, srv)
	client := newClient(t, srv)
	stream := startStream(t, client)
	fake.mu.Lock()
	fake.closed = "stopped"
	fake.mu.Unlock()
	_ = stream.WriteText("x")
	err := stream.Flush(context.Background())
	if !drim.IsCode(err, drim.CodeStreamClosed) {
		t.Fatalf("写入已结束的流应返回 stream_closed：%v", err)
	}
	<-stream.Context().Done()
	if !errors.Is(context.Cause(stream.Context()), drim.ErrStreamStopped) {
		t.Fatalf("Context 的原因 = %v", context.Cause(stream.Context()))
	}
	// 回调中的 ai.stream_stopped：本进程中找不到这个流时返回 false。
	if client.AI.StreamStopped("1841000000000000999") {
		t.Fatal("没有这个流")
	}
}

func sse(lines ...string) io.Reader {
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString(line)
		builder.WriteString("\n\n")
	}
	return strings.NewReader(builder.String())
}

func TestPipeAdapters(t *testing.T) {
	cases := []struct {
		name string
		pipe func(*drim.Stream, any) (*drim.AIStream, error)
		body io.Reader
		want []string
	}{
		{"openai", (*drim.Stream).PipeOpenAI, sse(
			`data: {"model":"gpt","choices":[{"delta":{"reasoning_content":"想"}}]}`,
			`data: {"choices":[{"delta":{"content":"你"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search"}}]}}]}`,
			`data: {"choices":[{"delta":{"content":"好"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, `data: [DONE]`),
			[]string{"reasoning:想", "text:你", "tool:done", "text:好"}},
		{"responses", (*drim.Stream).PipeOpenAIResponses, sse(
			`data: {"type":"response.reasoning_summary_text.delta","delta":"想"}`,
			`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc1","name":"search"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc1","name":"search"}}`,
			`data: {"type":"response.output_text.delta","delta":"你好"}`,
			`data: {"type":"response.completed","response":{"model":"gpt","usage":{"input_tokens":1,"output_tokens":2}}}`),
			[]string{"reasoning:想", "tool:done", "text:你好"}},
		{"anthropic", (*drim.Stream).PipeAnthropic, sse(
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"model":"claude","usage":{"input_tokens":3}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"search"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"你好"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`),
			[]string{"reasoning:想", "tool:done", "text:你好"}},
		{"gemini", (*drim.Stream).PipeGemini, sse(
			`data: {"candidates":[{"content":{"parts":[{"text":"想","thought":true}]}}],"modelVersion":"gemini"}`,
			`data: {"candidates":[{"content":{"parts":[{"text":"你"},{"text":"好"}]},"finishReason":"STOP"}]}`),
			[]string{"reasoning:想", "text:你好"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t)
			fake := newFakeAI(t, srv)
			client := newClient(t, srv)
			stream := startStream(t, client)
			if _, err := tc.pipe(stream, tc.body); err != nil {
				t.Fatal(err)
			}
			expectParts(t, fake.snapshot(), tc.want...)
		})
	}
}

// TestPipeModelError 模型返回错误时以 Fail 结束。
func TestPipeModelError(t *testing.T) {
	srv := newServer(t)
	newFakeAI(t, srv)
	client := newClient(t, srv)
	stream := startStream(t, client)
	_, err := stream.PipeOpenAI(sse(`data: {"choices":[{"delta":{"content":"你"}}]}`, `data: {"error":{"message":"overloaded"}}`))
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("模型错误 = %v", err)
	}
	if calls := srv.Calls("ai.fail_stream"); len(calls) != 1 || !strings.Contains(string(calls[0].Body), "model_error") {
		t.Fatalf("应以 model_error 报告失败：%v", calls)
	}
}

// TestPipeSpec 规格测试（与 Python、Node、Java 共用 testdata/ai）：录制的 SSE 被切成任意的块，Pipe* 转换出的片段和结束信息一致。
func TestPipeSpec(t *testing.T) {
	raw, err := os.ReadFile("testdata/ai/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Cases []struct {
			Name    string           `json:"name"`
			Adapter string           `json:"adapter"`
			File    string           `json:"file"`
			Parts   []map[string]any `json:"parts"`
			Finish  map[string]any   `json:"finish"`
			Error   string           `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	pipes := map[string]func(*drim.Stream, any) (*drim.AIStream, error){
		"openai": (*drim.Stream).PipeOpenAI, "openai_responses": (*drim.Stream).PipeOpenAIResponses,
		"anthropic": (*drim.Stream).PipeAnthropic, "gemini": (*drim.Stream).PipeGemini,
	}
	for _, c := range spec.Cases {
		for _, whole := range []bool{false, true} {
			name := c.Name + "/逐字节"
			if whole {
				name = c.Name + "/整个响应体"
			}
			t.Run(name, func(t *testing.T) {
				srv := newServer(t)
				fake := newFakeAI(t, srv)
				client := newClient(t, srv)
				stream := startStream(t, client)
				body, err := os.ReadFile("testdata/ai/" + c.File)
				if err != nil {
					t.Fatal(err)
				}
				var src io.Reader = iotest.OneByteReader(bytes.NewReader(body))
				if whole {
					src = bytes.NewReader(body)
				}
				_, err = pipes[c.Adapter](stream, src)
				if c.Error != "" {
					if err == nil || !strings.Contains(err.Error(), c.Error) {
						t.Fatalf("模型错误 = %v", err)
					}
					calls := srv.Calls("ai.fail_stream")
					if len(calls) != 1 || !strings.Contains(string(calls[0].Body), "model_error") || !strings.Contains(string(calls[0].Body), c.Error) {
						t.Fatalf("应以 model_error 报告失败：%v", calls)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					calls := srv.Calls("ai.finish_stream")
					var finish map[string]any
					if len(calls) != 1 || json.Unmarshal(calls[0].Body, &finish) != nil {
						t.Fatalf("结束：%v", calls)
					}
					delete(finish, "last_seq")
					if !reflect.DeepEqual(normalize(t, finish), normalize(t, c.Finish)) {
						t.Fatalf("结束信息 = %v，期望 %v", finish, c.Finish)
					}
				}
				if got, want := normalize(t, fake.snapshot()), normalize(t, c.Parts); !reflect.DeepEqual(got, want) {
					t.Fatalf("片段 = %v，期望 %v", got, want)
				}
			})
		}
	}
}

// normalize 经 JSON 往返，便于比较。
func normalize(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

// TestCallbackStopsLocalStream 回调处理器收到 ai.stream_stopped 时，本进程中的流自动取消 Context。
func TestCallbackStopsLocalStream(t *testing.T) {
	srv := newServer(t)
	newFakeAI(t, srv)
	client := newClient(t, srv)
	stream := startStream(t, client)
	var seen []string
	handler, err := callback.NewHandler(callback.Config{
		Secrets: callback.StaticSecrets{drimtest.DefaultEndpointID: {"cbs_test"}},
		Nonces:  callback.NewMemoryNonceStore(),
		Dedup:   callback.NewMemoryDedupStore(0),
		Events: callback.EventHandlers{AIStreamStopped: func(_ context.Context, _ *callback.Event, d *callback.AIStreamStopped) error {
			seen = append(seen, d.StreamID)
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("testdata/events/ai.stream_stopped.json")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, drimtest.SignedRequest(drimtest.DefaultEndpointID, body, "cbs_test"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	select {
	case <-stream.Context().Done():
	default:
		t.Fatal("流的 Context 应已取消")
	}
	if !errors.Is(context.Cause(stream.Context()), drim.ErrStreamStopped) || len(seen) != 1 || seen[0] != "1841000000000000123" {
		t.Fatalf("原因 = %v，处理函数收到 %v", context.Cause(stream.Context()), seen)
	}
}
