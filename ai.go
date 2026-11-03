package drim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/internal/aistop"
)

// AI 对话的流式回复（《AI对话/3.接口设计》第 7.1 节）：StartStream 开始一条回复，Write* 写入内容，Finish 或 Fail 结束；
// Pipe* 把主流模型的流式响应直接转为回复。SDK 先用一个持续写入的请求（NDJSON）发送增量，被断开或不可用时改用按批写入，
// 从最后确认的序号之后重发，内容不重复也不缺失。

// 增量的合批：满 20 毫秒或 4 KB 时作为一段发出。
const (
	streamFlushInterval = 20 * time.Millisecond
	streamFlushBytes    = 4 * 1024
	streamBatchMax      = 100
	streamBatchBytes    = 60 * 1024
	transportFailures   = 3
	// persistentHeaderTimeout 是持续写入等待响应头的时间。
	persistentHeaderTimeout = 5 * time.Second
)

// 停止的原因，ai.stream_stopped 和 stream_closed 时 Stream.Context 的 Cause。
var (
	// ErrStreamStopped 表示用户点了停止生成，或流已被平台结束（超时、被替换等）。
	ErrStreamStopped = errors.New("drim: 回复已被停止或结束")
)

// StartStreamInput 是 StartStream 的参数。
type StartStreamInput struct {
	Bot            string // 机器人的用户名
	To             string // 接收回复的用户名
	ReplyToSeq     int64  // 回复的是会话中的哪条消息，0 表示不引用
	ClientStreamID string // 幂等标识，不给时 SDK 生成
	Replace        bool   // 会话中已有进行中的回复时先结束它
	BatchOnly      bool   // 只用按批写入（运行环境不支持流式请求体时，如部分云函数）
}

// StartStream 开始一条流式回复，返回的 Stream 可以被多个协程同时写入。ctx 结束时停止发送，未结束的流由平台按超时结束；
// 用户停止生成时 Stream.Context() 被取消。
func (s *AIService) StartStream(ctx context.Context, in StartStreamInput) (*Stream, error) {
	params := CreateStreamParams{ClientStreamID: in.ClientStreamID, Replace: in.Replace}
	if in.ReplyToSeq > 0 {
		seq := in.ReplyToSeq
		params.ReplyToSeq = &seq
	}
	created, err := s.CreateStream(ctx, in.Bot, in.To, params)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	stream := &Stream{Info: created.Stream, ID: created.Stream.StreamID, svc: s, ctx: sctx, cancel: cancel, batchOnly: in.BatchOnly,
		wake: make(chan struct{}, 1), done: make(chan struct{}), acked: created.Stream.LastSeq, next: created.Stream.LastSeq + 1,
		lastIndex: -1, toolIndex: map[string]int{}}
	if created.Duplicate && created.Stream.Status != "streaming" {
		cancel(ErrStreamStopped)
		close(stream.done)
		return stream, nil
	}
	aistop.Register(stream.ID, stream.Stopped)
	go stream.run()
	return stream, nil
}

// StreamStopped 在收到 ai.stream_stopped 时调用：本进程中有这个流时取消它的 Context，模型调用随之中止。
// 返回是否找到了流（流在其他进程中时为 false，那边的写入会在下一次发送时收到 stream_closed）。
// callback.Handler 收到这个事件时会自动调用，自己处理回调（如从队列取出事件）时才需要调用它。
func (s *AIService) StreamStopped(streamID string) bool { return aistop.Stop(streamID) }

// Stream 是一条进行中的流式回复。
type Stream struct {
	// Info 是开始时的流对象。
	Info AIStream
	// ID 是流 ID，开发者在 ai.stream_stopped 回调中按它找到流。
	ID string

	svc       *AIService
	ctx       context.Context
	cancel    context.CancelCauseFunc
	batchOnly bool

	mu        sync.Mutex
	next      int64     // 下一段的序号
	acked     int64     // 已确认的最后一段
	queue     []AIDelta // 已编号、未确认的段
	ops       []AIOp    // 还没编号的操作（合批中）
	opsBytes  int
	opsSince  time.Time
	parts     int // 已经出现的片段数
	lastIndex int
	lastType  string
	toolIndex map[string]int
	err       error // 写入失败（不可恢复）的原因
	closing   bool
	wake      chan struct{}
	done      chan struct{}
	waiters   []chan struct{}
}

// Context 在用户停止生成、流被平台结束或写入失败时取消，context.Cause 给出原因。开发者把它传给模型的请求，停止时模型调用随之中止。
func (s *Stream) Context() context.Context { return s.ctx }

// WriteText 追加正文（Markdown）。
func (s *Stream) WriteText(text string) error { return s.appendText("text", text) }

// WriteReasoning 追加思考过程。
func (s *Stream) WriteReasoning(text string) error { return s.appendText("reasoning", text) }

// ImagePart 是图片片段。
type ImagePart struct {
	URL    string // https 地址，建议先用媒体模块上传
	Width  int64
	Height int64
	Alt    string
}

// WriteImage 写入一张图片。
func (s *Stream) WriteImage(image ImagePart) error {
	return s.put(-1, AIPart{Type: "image", URL: image.URL, Width: image.Width, Height: image.Height, Alt: image.Alt})
}

// FilePart 是文件片段。
type FilePart struct {
	URL      string
	Name     string
	Size     int64
	MimeType string
}

// WriteFile 写入一个文件。
func (s *Stream) WriteFile(file FilePart) error {
	return s.put(-1, AIPart{Type: "file", URL: file.URL, Name: file.Name, Size: file.Size, MimeType: file.MimeType})
}

// 工具调用的状态。
const (
	ToolRunning = "running"
	ToolDone    = "done"
	ToolFailed  = "failed"
)

// WriteTool 写入工具调用的进度：同一个 id 的再次写入更新同一个片段（如 running 变为 done）。只用于展示，不含参数和结果。
func (s *Stream) WriteTool(id, name, title, state string) error {
	s.mu.Lock()
	index, ok := s.toolIndex[id]
	s.mu.Unlock()
	if !ok {
		index = -1
	}
	return s.put(index, AIPart{Type: "tool", ID: id, Name: name, Title: title, State: state})
}

func (s *Stream) appendText(partType, text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(); err != nil {
		return err
	}
	index := s.lastIndex
	if s.lastType != partType || index < 0 {
		index = s.parts
		s.parts++
		s.lastIndex, s.lastType = index, partType
	}
	if n := len(s.ops); n > 0 && s.ops[n-1].Op == "append" && s.ops[n-1].Index == int64(index) {
		s.ops[n-1].Text += text
	} else {
		s.ops = append(s.ops, AIOp{Op: "append", Index: int64(index), Type: partType, Text: text})
	}
	s.queuedLocked(len(text))
	return nil
}

// put 整体设置片段；index 为 -1 时新建。
func (s *Stream) put(index int, part AIPart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(); err != nil {
		return err
	}
	if index < 0 {
		index = s.parts
		s.parts++
	}
	if part.Type == "tool" {
		s.toolIndex[part.ID] = index
	}
	s.lastIndex, s.lastType = index, part.Type
	copied := part
	s.ops = append(s.ops, AIOp{Op: "put", Index: int64(index), Part: &copied})
	s.queuedLocked(len(part.URL) + len(part.Name) + len(part.Title) + 64)
	return nil
}

func (s *Stream) writableLocked() error {
	switch {
	case s.err != nil:
		return s.err
	case s.closing:
		return newError(CodeConflict, "回复已结束，不能再写入")
	case s.ctx.Err() != nil:
		return context.Cause(s.ctx)
	}
	return nil
}

// queuedLocked 记下合批中的大小，满 4 KB 时立即唤醒发送。
func (s *Stream) queuedLocked(size int) {
	if len(s.ops) == 1 {
		s.opsSince = time.Now()
	}
	s.opsBytes += size
	if s.opsBytes >= streamFlushBytes {
		s.sealLocked()
	}
	s.signal()
}

// sealLocked 把合批中的操作编号为一段，放入待发送的队列。
func (s *Stream) sealLocked() {
	if len(s.ops) == 0 {
		return
	}
	s.queue = append(s.queue, AIDelta{Seq: s.next, Ops: s.ops})
	s.next++
	s.ops, s.opsBytes = nil, 0
}

func (s *Stream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Flush 立即发出已写入的内容，等待服务端确认。
func (s *Stream) Flush(ctx context.Context) error {
	s.mu.Lock()
	s.sealLocked()
	target := s.next - 1
	s.mu.Unlock()
	s.signal()
	return s.waitAcked(ctx, target)
}

// waitAcked 等待 target 及之前的段都被确认，或者写入失败。
func (s *Stream) waitAcked(ctx context.Context, target int64) error {
	for {
		s.mu.Lock()
		if s.acked >= target {
			s.mu.Unlock()
			return nil
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return err
		}
		ch := make(chan struct{})
		s.waiters = append(s.waiters, ch)
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
		}
	}
}

func (s *Stream) notifyLocked() {
	for _, ch := range s.waiters {
		close(ch)
	}
	s.waiters = nil
}

// FinishOptions 是结束的可选信息。
type FinishOptions struct {
	FinishReason string // 如 stop、length、tool_calls，默认 stop
	Model        string
	Usage        *AIUsage
}

// Finish 发出全部内容并等待确认后结束流（已完成）。流已被用户停止或平台结束时返回 *Error（stream_closed）。
func (s *Stream) Finish(ctx context.Context, opts FinishOptions) (*AIStream, error) {
	if err := s.Flush(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.closing = true
	last := s.acked
	s.mu.Unlock()
	defer s.shutdown(nil)
	result, err := s.svc.FinishStream(ctx, s.ID, last, FinishStreamParams{FinishReason: opts.FinishReason, Model: opts.Model, Usage: opts.Usage})
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Retried && e.Code == CodeStreamClosed && e.Details["reason"] == "completed" {
			// 超时后重试：第一次已经结束了。
			got, gerr := s.svc.GetStream(ctx, s.ID)
			if gerr == nil {
				return &got.Stream, nil
			}
		}
		return nil, err
	}
	return &result.Stream, nil
}

// Fail 报告失败：以“生成中断”结束，已生成的部分保留，客户端显示 message。已发出但未确认的内容尽量先确认。
func (s *Stream) Fail(ctx context.Context, code, message string) (*AIStream, error) {
	flushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = s.Flush(flushCtx)
	cancel()
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	defer s.shutdown(nil)
	result, err := s.svc.FailStream(ctx, s.ID, code, message)
	if err != nil {
		return nil, err
	}
	return &result.Stream, nil
}

// Close 停止发送并释放资源，没有调用 Finish 或 Fail 时以 Fail("canceled") 结束。可以重复调用。
func (s *Stream) Close() error {
	s.mu.Lock()
	finished := s.closing
	s.mu.Unlock()
	if !finished && s.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = s.Fail(ctx, "canceled", "")
	}
	s.shutdown(nil)
	return nil
}

// Stopped 由回调处理器在收到 ai.stream_stopped 时调用：取消 Stream.Context，停止发送。
func (s *Stream) Stopped() { s.shutdown(ErrStreamStopped) }

func (s *Stream) shutdown(cause error) {
	aistop.Unregister(s.ID)
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.notifyLocked()
	s.mu.Unlock()
	if cause == nil {
		cause = context.Canceled
	}
	s.cancel(cause)
}

// fail 记下不可恢复的写入错误：流已结束（stream_closed）、增量格式不对、累积内容超过上限等。
func (s *Stream) failWith(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.notifyLocked()
	s.mu.Unlock()
	cause := err
	var e *Error
	if errors.As(err, &e) && e.Code == CodeStreamClosed {
		cause = ErrStreamStopped
	}
	s.cancel(cause)
}

// run 是发送的协程：合批计时，先用持续写入，失败后改用按批写入，连续失败 3 次时以 Fail("transport") 结束。
func (s *Stream) run() {
	failures := 0
	persistent := !s.batchOnly
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		case <-timer.C:
		}
		s.mu.Lock()
		if len(s.ops) > 0 && time.Since(s.opsSince) >= streamFlushInterval {
			s.sealLocked()
		}
		pendingOps := len(s.ops) > 0
		since := s.opsSince
		queued := len(s.queue) > 0
		s.mu.Unlock()
		if pendingOps {
			wait := streamFlushInterval - time.Since(since)
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			timer.Reset(wait)
		}
		if !queued {
			continue
		}
		var err error
		if persistent {
			err = s.sendPersistent()
			if errors.Is(err, errPersistentUnavailable) {
				persistent = false
				s.signal()
				continue
			}
		} else {
			err = s.sendBatch()
		}
		if err == nil {
			failures = 0
			continue
		}
		if permanent(err) {
			s.failWith(err)
			return
		}
		failures++
		persistent = false // 持续写入失败后改用按批写入
		if failures >= transportFailures {
			// 先报告失败再让等待的调用返回：调用方收到错误时流已经结束
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = s.svc.FailStream(ctx, s.ID, "transport", "")
			cancel()
			s.failWith(err)
			return
		}
		select {
		case <-s.done:
			return
		case <-time.After(backoff(failures, 0)):
		}
		s.signal()
	}
}

// errPersistentUnavailable 表示持续写入的接口不可用，改用按批写入。
var errPersistentUnavailable = errors.New("drim: 持续写入不可用")

// permanent 判断写入错误不能靠重发解决。
func permanent(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case CodeStreamClosed, CodeInvalidArgument, CodePayloadTooLarge, CodeNotFound, CodeUnauthenticated, CodePermissionDenied:
		return true
	}
	return false
}

// takeBatch 取出待确认的段（从最早的未确认段开始），最多 100 段、约 60 KB。
func (s *Stream) takeBatch() []AIDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var batch []AIDelta
	size := 0
	for _, delta := range s.queue {
		if delta.Seq <= s.acked {
			continue
		}
		cost := 64
		for _, op := range delta.Ops {
			cost += len(op.Text) + 64
		}
		if len(batch) > 0 && (len(batch) >= streamBatchMax || size+cost > streamBatchBytes) {
			break
		}
		batch = append(batch, delta)
		size += cost
	}
	return batch
}

// ack 记下服务端确认的序号，丢掉已确认的段。
func (s *Stream) ack(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.acked {
		s.acked = seq
	}
	kept := s.queue[:0]
	for _, delta := range s.queue {
		if delta.Seq > s.acked {
			kept = append(kept, delta)
		}
	}
	s.queue = kept
	s.notifyLocked()
}

// sendBatch 用按批写入发送全部待确认的段。服务端跳过已接受的段，所以重发不会重复。
func (s *Stream) sendBatch() error {
	for {
		batch := s.takeBatch()
		if len(batch) == 0 {
			return nil
		}
		result, err := s.svc.WriteDeltas(s.ctx, s.ID, batch)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == CodeSeqGap {
				// 服务端缺少更早的段（不应发生）：从它期望的序号重发。
				if expected, ok := e.Details["expected_seq"].(float64); ok {
					s.ack(int64(expected) - 1)
					continue
				}
			}
			return err
		}
		s.ack(result.AckedSeq)
	}
}

// sendPersistent 用一个持续写入的请求发送：请求体逐行写出增量，响应逐行确认；队列空了以后保持连接，等待新的段，
// 直到流结束或连接断开。
func (s *Stream) sendPersistent() error {
	token, terr := s.svc.t.tokens.get(s.ctx)
	if terr != nil {
		return terr
	}
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.svc.t.prefix+"/ai/streams/"+s.ID+"/deltas", reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("User-Agent", s.svc.t.userAgent)
	req.Header.Set("X-Request-ID", newRequestID())
	errs := make(chan error, 2)
	var sent int64
	go func() {
		errs <- s.pump(ctx, writer, &sent)
	}()
	// 服务端收到请求后立即返回响应头；5 秒内没有收到，说明中间的代理在缓冲请求体，持续写入不可用。
	headerTimer := time.AfterFunc(persistentHeaderTimeout, cancel)
	resp, err := s.svc.t.http.Do(req)
	stopped := headerTimer.Stop()
	if err != nil {
		writer.CloseWithError(err)
		if !stopped && s.ctx.Err() == nil {
			return errPersistentUnavailable
		}
		return newError(CodeNetworkError, "持续写入的请求失败："+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := readLimited(resp.Body, 64*1024)
		writer.CloseWithError(errors.New("closed"))
		e := errorFromResponse(resp.StatusCode, resp.Header, data, resp.Header.Get("X-Request-ID"))
		if e.Code == CodeStreamClosed {
			return e
		}
		// 持续写入不可用（代理不支持、服务端版本较旧等）：改用按批写入，不计入失败次数。
		return errPersistentUnavailable
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var reply struct {
			AckedSeq *int64          `json:"acked_seq"`
			Error    json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &reply) != nil {
			continue
		}
		if len(reply.Error) > 0 {
			writer.CloseWithError(errors.New("closed"))
			body, _ := json.Marshal(map[string]json.RawMessage{"error": reply.Error})
			e := errorFromResponse(statusOfLine(reply.Error), resp.Header, body, resp.Header.Get("X-Request-ID"))
			if e.Code == CodeSeqGap {
				if expected, ok := e.Details["expected_seq"].(float64); ok {
					s.ack(int64(expected) - 1)
				}
				return newError(CodeNetworkError, "持续写入的序号不连续，重新发送")
			}
			return e
		}
		if reply.AckedSeq != nil {
			s.ack(*reply.AckedSeq)
		}
	}
	writer.Close()
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
			return newError(CodeNetworkError, "持续写入中断："+err.Error())
		}
	default:
	}
	s.mu.Lock()
	remaining := len(s.queue)
	closing := s.closing
	s.mu.Unlock()
	if remaining > 0 && !closing {
		return newError(CodeNetworkError, "持续写入的连接被关闭，未确认的内容改用按批写入")
	}
	return nil
}

// statusOfLine 按错误码换算 HTTP 状态码，供 errorFromResponse 使用。
func statusOfLine(raw json.RawMessage) int {
	var object struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &object)
	switch object.Code {
	case CodeStreamClosed, CodeSeqGap, CodeStreamInProgress:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeRateLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// pump 把待确认的段逐行写入请求体；已写出的段记在 sent 中，断开重连后从未确认的段开始。流结束（Finish、Fail）时关闭请求体。
func (s *Stream) pump(ctx context.Context, writer *io.PipeWriter, sent *int64) error {
	defer writer.Close()
	*sent = s.currentAcked()
	for {
		s.mu.Lock()
		if len(s.ops) > 0 && time.Since(s.opsSince) >= streamFlushInterval {
			s.sealLocked()
		}
		var lines []AIDelta
		for _, delta := range s.queue {
			if delta.Seq > *sent {
				lines = append(lines, delta)
			}
		}
		closing := s.closing && len(lines) == 0 && len(s.ops) == 0
		s.mu.Unlock()
		for _, delta := range lines {
			w := &jw{}
			encAIDelta(w, delta)
			if w.err != nil {
				return w.err
			}
			line := append(w.buf, '\n')
			if _, err := writer.Write(line); err != nil {
				return err
			}
			*sent = delta.Seq
		}
		if closing {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *Stream) currentAcked() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// String 返回流 ID，便于日志。
func (s *Stream) String() string {
	return "ai stream " + s.ID + " (seq " + strconv.FormatInt(s.currentAcked(), 10) + ")"
}
