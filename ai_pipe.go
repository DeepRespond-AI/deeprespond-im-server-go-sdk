package drim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
)

// 对接主流模型的流式响应（《AI对话/3.接口设计》第 7.1 节“转换”）。Pipe* 读完模型的流后自动 Finish，模型返回错误时自动
// Fail("model_error")；用户停止生成时 Stream.Context() 被取消，Pipe* 停止读取并关闭模型的流。
//
// src 可以是官方 SDK 的流（有 Next() bool、Current() T、Err() error 方法，如 openai-go 的 *ssestream.Stream[T]、
// anthropic-sdk-go 的 *ssestream.Stream[T]），也可以是 HTTP 响应体（io.Reader，SSE 格式的原始响应）。
// 兼容 OpenAI 格式的模型（DeepSeek、通义千问、Kimi、豆包、vLLM、Ollama 等）都用 PipeOpenAI。

// PipeOpenAI 对接 OpenAI Chat Completions 的流：delta.content 写为正文，delta.reasoning_content 写为思考过程，tool_calls 写为工具调用。
func (s *Stream) PipeOpenAI(src any) (*AIStream, error) { return s.pipe(src, &openAIChat{}) }

// PipeOpenAIResponses 对接 OpenAI Responses API 的流。
func (s *Stream) PipeOpenAIResponses(src any) (*AIStream, error) {
	return s.pipe(src, &openAIResponses{})
}

// PipeAnthropic 对接 Anthropic Messages 的流：text_delta 写为正文，thinking_delta 写为思考过程，tool_use 写为工具调用。
func (s *Stream) PipeAnthropic(src any) (*AIStream, error) { return s.pipe(src, &anthropicStream{}) }

// PipeGemini 对接 Gemini generateContent 的流：thought 为真的部分写为思考过程，其余写为正文。
func (s *Stream) PipeGemini(src any) (*AIStream, error) { return s.pipe(src, &geminiStream{}) }

// converter 把模型的一个流式块（JSON 对象）转为写入。
type converter interface {
	apply(s *Stream, chunk map[string]any) error
	finish() FinishOptions
}

// toolTracker 记下还在进行中的工具调用，模型的流结束时把它们改为 done（Chat Completions 没有工具调用结束的事件）。
type toolTracker struct {
	running map[string]string // id → 名称
}

func (t *toolTracker) start(s *Stream, id, name string) error {
	if id == "" {
		return nil
	}
	if t.running == nil {
		t.running = map[string]string{}
	}
	if _, ok := t.running[id]; ok {
		return nil
	}
	t.running[id] = name
	return s.WriteTool(id, name, name, ToolRunning)
}

func (t *toolTracker) done(s *Stream, id string) error {
	name, ok := t.running[id]
	if !ok {
		return nil
	}
	delete(t.running, id)
	return s.WriteTool(id, name, name, ToolDone)
}

// completeAll 把还在进行中的工具调用改为 done。
func (t *toolTracker) completeAll(s *Stream) error {
	for id := range t.running {
		if err := t.done(s, id); err != nil {
			return err
		}
	}
	return nil
}

// toolsOf 返回转换器的工具调用记录。
func toolsOf(conv converter) *toolTracker {
	switch c := conv.(type) {
	case *openAIChat:
		return &c.tracker
	case *openAIResponses:
		return &c.tracker
	case *anthropicStream:
		return &c.tracker
	}
	return nil
}

func (s *Stream) pipe(src any, conv converter) (*AIStream, error) {
	next, closeSrc, err := chunkSource(src)
	if err != nil {
		return nil, err
	}
	defer closeSrc()
	done := make(chan struct{})
	go func() {
		select {
		case <-s.ctx.Done():
			closeSrc()
		case <-done:
		}
	}()
	defer close(done)
	for {
		chunk, ok, err := next()
		if s.ctx.Err() != nil {
			return nil, context.Cause(s.ctx)
		}
		if err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = s.Fail(ctx, "model_error", truncate(err.Error(), 256))
			return nil, err
		}
		if !ok {
			break
		}
		if err := conv.apply(s, chunk); err != nil {
			return nil, err
		}
	}
	if tracker := toolsOf(conv); tracker != nil {
		if err := tracker.completeAll(s); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Finish(ctx, conv.finish())
}

// chunkSource 返回逐个读取流式块的函数和关闭函数。
func chunkSource(src any) (func() (map[string]any, bool, error), func(), error) {
	if reader, ok := src.(io.Reader); ok {
		return sseChunks(reader)
	}
	value := reflect.ValueOf(src)
	nextMethod := value.MethodByName("Next")
	currentMethod := value.MethodByName("Current")
	errMethod := value.MethodByName("Err")
	if !nextMethod.IsValid() || !currentMethod.IsValid() || !errMethod.IsValid() {
		return nil, nil, invalidParams("src", "src 应为 io.Reader（SSE 格式的响应体），或有 Next、Current、Err 方法的流")
	}
	closeMethod := value.MethodByName("Close")
	closeFn := func() {
		if closeMethod.IsValid() {
			closeMethod.Call(nil)
		}
	}
	next := func() (map[string]any, bool, error) {
		if !nextMethod.Call(nil)[0].Bool() {
			if errValue := errMethod.Call(nil)[0]; !errValue.IsNil() {
				return nil, false, errValue.Interface().(error)
			}
			return nil, false, nil
		}
		raw, err := json.Marshal(currentMethod.Call(nil)[0].Interface())
		if err != nil {
			return nil, false, err
		}
		var chunk map[string]any
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return nil, false, err
		}
		return chunk, true, nil
	}
	return next, closeFn, nil
}

// sseChunks 读取 SSE 格式的响应：每个 data: 行为一个 JSON 块，[DONE] 结束；Anthropic 的 event: 行写入块的 type。
func sseChunks(reader io.Reader) (func() (map[string]any, bool, error), func(), error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	closeFn := func() {
		if closer, ok := reader.(io.Closer); ok {
			closer.Close()
		}
	}
	next := func() (map[string]any, bool, error) {
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				return nil, false, nil
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if errObject, ok := chunk["error"].(map[string]any); ok {
				message, _ := errObject["message"].(string)
				return nil, false, errors.New("模型返回错误：" + message)
			}
			return chunk, true, nil
		}
		return nil, false, scanner.Err()
	}
	return next, closeFn, nil
}

// 读取 JSON 的辅助函数。
func object(value any, keys ...string) map[string]any {
	current, _ := value.(map[string]any)
	for _, key := range keys {
		current, _ = current[key].(map[string]any)
	}
	return current
}

func array(value any, key string) []any {
	items, _ := object(value)[key].([]any)
	return items
}

func str(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func num(value map[string]any, key string) int64 {
	number, _ := value[key].(float64)
	return int64(number)
}

func truncate(text string, runes int) string {
	if r := []rune(text); len(r) > runes {
		return string(r[:runes])
	}
	return text
}

// usageOf 读取 OpenAI 格式的 usage。
func usageOf(value map[string]any, inputKey, outputKey string) *AIUsage {
	if value == nil {
		return nil
	}
	return &AIUsage{InputTokens: num(value, inputKey), OutputTokens: num(value, outputKey)}
}

// openAIChat 转换 Chat Completions 的块。
type openAIChat struct {
	reason  string
	model   string
	usage   *AIUsage
	tools   map[int64]string // 工具调用的序号 → id
	tracker toolTracker
}

func (c *openAIChat) apply(s *Stream, chunk map[string]any) error {
	if model := str(chunk, "model"); model != "" {
		c.model = model
	}
	if usage := object(chunk, "usage"); usage != nil {
		c.usage = usageOf(usage, "prompt_tokens", "completion_tokens")
	}
	for _, item := range array(chunk, "choices") {
		choice := object(item)
		if reason := str(choice, "finish_reason"); reason != "" {
			c.reason = reason
		}
		delta := object(choice, "delta")
		if reasoning := str(delta, "reasoning_content"); reasoning != "" {
			if err := s.WriteReasoning(reasoning); err != nil {
				return err
			}
		}
		if reasoning := str(delta, "reasoning"); reasoning != "" {
			if err := s.WriteReasoning(reasoning); err != nil {
				return err
			}
		}
		if content := str(delta, "content"); content != "" {
			if err := s.WriteText(content); err != nil {
				return err
			}
		}
		for _, raw := range array(delta, "tool_calls") {
			call := object(raw)
			if c.tools == nil {
				c.tools = map[int64]string{}
			}
			index := num(call, "index")
			id := str(call, "id")
			if id == "" {
				id = c.tools[index]
			}
			name := str(object(call, "function"), "name")
			if _, seen := c.tools[index]; !seen && id != "" {
				c.tools[index] = id
				if err := c.tracker.start(s, id, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *openAIChat) finish() FinishOptions {
	return FinishOptions{FinishReason: c.reason, Model: c.model, Usage: c.usage}
}

// openAIResponses 转换 Responses API 的事件。
type openAIResponses struct {
	reason  string
	model   string
	usage   *AIUsage
	tracker toolTracker
}

func (c *openAIResponses) apply(s *Stream, chunk map[string]any) error {
	switch str(chunk, "type") {
	case "response.output_text.delta":
		return s.WriteText(str(chunk, "delta"))
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return s.WriteReasoning(str(chunk, "delta"))
	case "response.output_item.added":
		item := object(chunk, "item")
		if t := str(item, "type"); t == "function_call" || strings.HasSuffix(t, "_call") {
			name := str(item, "name")
			if name == "" {
				name = t
			}
			return c.tracker.start(s, str(item, "id"), name)
		}
	case "response.output_item.done":
		item := object(chunk, "item")
		if t := str(item, "type"); t == "function_call" || strings.HasSuffix(t, "_call") {
			return c.tracker.done(s, str(item, "id"))
		}
	case "response.completed", "response.incomplete":
		response := object(chunk, "response")
		c.model = str(response, "model")
		if usage := object(response, "usage"); usage != nil {
			c.usage = usageOf(usage, "input_tokens", "output_tokens")
		}
		c.reason = "stop"
		if str(chunk, "type") == "response.incomplete" {
			c.reason = str(object(response, "incomplete_details"), "reason")
		}
	}
	return nil
}

func (c *openAIResponses) finish() FinishOptions {
	return FinishOptions{FinishReason: c.reason, Model: c.model, Usage: c.usage}
}

// anthropicStream 转换 Anthropic Messages 的事件。
type anthropicStream struct {
	reason  string
	model   string
	input   int64
	output  int64
	blocks  map[int64]string // 内容块的序号 → 工具调用的 id
	tracker toolTracker
}

func (c *anthropicStream) apply(s *Stream, chunk map[string]any) error {
	switch str(chunk, "type") {
	case "message_start":
		message := object(chunk, "message")
		c.model = str(message, "model")
		c.input = num(object(message, "usage"), "input_tokens")
	case "content_block_start":
		block := object(chunk, "content_block")
		if t := str(block, "type"); t == "tool_use" || t == "server_tool_use" {
			if c.blocks == nil {
				c.blocks = map[int64]string{}
			}
			c.blocks[num(chunk, "index")] = str(block, "id")
			return c.tracker.start(s, str(block, "id"), str(block, "name"))
		}
	case "content_block_stop":
		if id, ok := c.blocks[num(chunk, "index")]; ok {
			return c.tracker.done(s, id)
		}
	case "content_block_delta":
		delta := object(chunk, "delta")
		switch str(delta, "type") {
		case "text_delta":
			return s.WriteText(str(delta, "text"))
		case "thinking_delta":
			return s.WriteReasoning(str(delta, "thinking"))
		}
	case "message_delta":
		if reason := str(object(chunk, "delta"), "stop_reason"); reason != "" {
			c.reason = reason
		}
		if usage := object(chunk, "usage"); usage != nil {
			c.output = num(usage, "output_tokens")
		}
	}
	return nil
}

func (c *anthropicStream) finish() FinishOptions {
	return FinishOptions{FinishReason: c.reason, Model: c.model, Usage: &AIUsage{InputTokens: c.input, OutputTokens: c.output}}
}

// geminiStream 转换 Gemini 的块。
type geminiStream struct {
	reason string
	model  string
	usage  *AIUsage
}

func (c *geminiStream) apply(s *Stream, chunk map[string]any) error {
	if model := str(chunk, "modelVersion"); model != "" {
		c.model = model
	}
	if model := str(chunk, "model_version"); model != "" {
		c.model = model
	}
	usage := object(chunk, "usageMetadata")
	if usage == nil {
		usage = object(chunk, "usage_metadata")
	}
	if usage != nil {
		c.usage = &AIUsage{InputTokens: num(usage, "promptTokenCount") + num(usage, "prompt_token_count"),
			OutputTokens: num(usage, "candidatesTokenCount") + num(usage, "candidates_token_count")}
	}
	for _, raw := range array(chunk, "candidates") {
		candidate := object(raw)
		if reason := str(candidate, "finishReason"); reason != "" {
			c.reason = strings.ToLower(reason)
		}
		if reason := str(candidate, "finish_reason"); reason != "" {
			c.reason = strings.ToLower(reason)
		}
		for _, item := range array(object(candidate, "content"), "parts") {
			part := object(item)
			text := str(part, "text")
			if text == "" {
				continue
			}
			thought, _ := part["thought"].(bool)
			var err error
			if thought {
				err = s.WriteReasoning(text)
			} else {
				err = s.WriteText(text)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *geminiStream) finish() FinishOptions {
	return FinishOptions{FinishReason: c.reason, Model: c.model, Usage: c.usage}
}
