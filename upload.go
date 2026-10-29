package drim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// 文件的上传与下载（《1.功能概述文档》第 10 节，《2.接口设计》第 8 节）。

const (
	partSize          = 16 << 20
	avatarMax         = 5 << 20
	presignBatch      = 100
	completingTotal   = 3 * time.Minute
	cancelTimeout     = 5 * time.Second
	objectPutTimeout  = 5 * time.Minute
	maxChangedRetries = 3
)

var (
	putDelays        = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	completingDelays = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	purposeKinds     = map[string][]string{
		"attachment":      {"image", "voice", "video", "file"},
		"group_file":      {"image", "video", "file"},
		"user_avatar":     {"image"},
		"group_avatar":    {"image"},
		"chatroom_avatar": {"image"},
	}
	errExpired = errors.New("对象存储返回 403：地址已过期")
	// resumePartSize 续传时的分片大小（服务端固定为 16 MB；测试中可以改小）
	resumePartSize int64 = partSize
)

// UploadParams 是上传的参数。
type UploadParams struct {
	Purpose     string // attachment、user_avatar、group_avatar、chatroom_avatar、group_file
	Kind        string // image、voice、video、file
	Name        string // 类型为 file 和群文件必填；最长 255，不含 / \ 和控制字符
	ContentType string // 只作提示，格式由服务端按内容识别
	GroupID     string // 群文件的群
	Owner       string // 用户头像的所属用户（必填），其他用途可选
	Concurrency int    // 分片并发，默认 4
	// OnProgress 每个分片（一次上传为整个文件）完成后调用，同一时刻只有一个协程调用，不能阻塞。
	OnProgress func(uploaded, total int64)
}

// Upload 上传文件：创建上传、上传内容（不超过 32 MB 一次上传，更大的分片并发上传）、完成上传，返回文件信息。
//
// r 用于分片的并发读取和重传（*os.File、*bytes.Reader、*strings.Reader 都实现了 io.ReaderAt；只有 io.Reader 的先写入临时文件）。
// 上传内容的请求发到对象存储：带上返回的请求头（Content-Length），不带 Authorization。完成上传的各种结果按第 10 节的表处理。
// ctx 被取消或到期时停止上传，并尽力取消这次上传（独立的 5 秒时限，失败不报错）。
// File.URL 是文件地址，写入消息的 Body 或资料的 AvatarURL；不要保存下载地址。
func (s *MediaService) Upload(ctx context.Context, r io.ReaderAt, size int64, params UploadParams) (*File, error) {
	if err := checkUpload(r, size, params); err != nil {
		return nil, s.t.localError("media.upload", err)
	}
	session, err := s.CreateUpload(ctx, CreateUploadParams{
		Purpose: params.Purpose, Kind: params.Kind, Size: size, ContentType: params.ContentType,
		Name: params.Name, GroupID: params.GroupID, Owner: params.Owner,
	})
	if err != nil {
		return nil, err
	}
	u := s.uploader(r, size, session.FileID, params)
	file, err := u.run(ctx, session)
	if err != nil && ctx.Err() != nil {
		s.cancelQuietly(ctx, session.FileID)
	}
	return file, err
}

// ResumeUpload 分片上传中断后续传（同一份内容，分片上传 24 小时内）：查询已上传的分片，补传缺少的和大小不对的，再完成。
// ctx 被取消时只停止，不取消上传（之后还可以再续传）。
func (s *MediaService) ResumeUpload(ctx context.Context, fileID string, r io.ReaderAt, size int64, params UploadParams) (*File, error) {
	if r == nil || size <= 0 {
		return nil, s.t.localError("media.upload", invalidParams("source", "续传的内容不能为空"))
	}
	u := s.uploader(r, size, fileID, params)
	missing, err := u.missingParts(ctx, resumePartSize)
	if err != nil {
		return nil, err
	}
	u.progress.add(size - u.bytesOf(missing, resumePartSize))
	if len(missing) > 0 {
		if err := u.uploadParts(ctx, resumePartSize, missing); err != nil {
			return nil, err
		}
	}
	return u.complete(ctx, resumePartSize, true)
}

// Download 换取下载地址后直接请求（不带 Authorization），把内容写入 w，返回写入的字节数。
// 不经过 OpenAPI 的重试和限流（只有换取下载地址那一步经过）；下载地址过期（对象存储返回 403）时重新换取一次。
// 缩略图的地址（文件地址加 /thumb）同样可以下载。
func (s *MediaService) Download(ctx context.Context, url string, w io.Writer) (int64, error) {
	for attempt := 0; attempt < 2; attempt++ {
		items, err := s.DownloadURLs(ctx, []string{url})
		if err != nil {
			return 0, err
		}
		if len(items) == 0 {
			return 0, s.t.localError("media.download", newError(CodeNotFound, "换取下载地址失败"))
		}
		if item := items[0]; item.Error != nil {
			e := newError(item.Error.Code, item.Error.Message)
			e.Details = item.Error.Details
			return 0, s.t.localError("media.download", e)
		}
		n, err := s.fetch(ctx, items[0].DownloadURL, w)
		if errors.Is(err, errExpired) && n == 0 {
			continue
		}
		return n, err
	}
	e := newError(CodeNetworkError, "下载地址反复过期（对象存储返回 403）")
	return 0, s.t.localError("media.download", e)
}

func (s *MediaService) fetch(ctx context.Context, url string, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, s.t.localError("media.download", invalidParams("url", "下载地址不合法："+err.Error()))
	}
	resp, err := s.t.http.Do(req)
	if err != nil {
		f := classify(ctx, ctx, err, true)
		return 0, s.t.localError("media.download", f.err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return 0, errExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		prefix, _ := readLimited(resp.Body, maxBodyPrefix)
		e := newError(CodeInvalidResponse, fmt.Sprintf("下载失败（HTTP %d）", resp.StatusCode))
		e.StatusCode = resp.StatusCode
		e.Details = map[string]any{"body_prefix": bodyPrefix(prefix)}
		return 0, s.t.localError("media.download", e)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return n, s.t.localError("media.download", ctxError(ctx))
		}
		e := newError(CodeNetworkError, "下载中断："+err.Error())
		e.Err = err
		return n, s.t.localError("media.download", e)
	}
	return n, nil
}

// localError 补全 SDK 在 OpenAPI 之外的步骤（上传到对象存储、下载）的错误。
func (t *transport) localError(op string, e *Error) *Error {
	e.Op = op
	if e.Attempts == 0 {
		e.Attempts = 1
	}
	t.log.Debug("请求失败", "op", op, "code", e.Code, "status", e.StatusCode)
	return e
}

func checkUpload(r io.ReaderAt, size int64, p UploadParams) *Error {
	kinds, ok := purposeKinds[p.Purpose]
	if !ok {
		return invalidParams("purpose", "不认识的用途 "+strconv.Quote(p.Purpose))
	}
	allowed := false
	for _, k := range kinds {
		allowed = allowed || k == p.Kind
	}
	if !allowed {
		return invalidParams("kind", "用途 "+p.Purpose+" 不能上传 "+strconv.Quote(p.Kind))
	}
	if r == nil || size <= 0 {
		return invalidParams("source", "不能上传空文件")
	}
	if strings.HasSuffix(p.Purpose, "_avatar") && size > avatarMax {
		e := invalidParams("size", "头像不能超过 5 MB")
		e.Details["max_bytes"] = avatarMax
		return e
	}
	if (p.Kind == "file" || p.Purpose == "group_file") && p.Name == "" {
		return invalidParams("name", "类型为 file 和群文件必须给出文件名")
	}
	if p.Purpose == "group_file" && p.GroupID == "" {
		return invalidParams("group_id", "群文件必须给出 GroupID")
	}
	if p.Purpose == "user_avatar" && p.Owner == "" {
		return invalidParams("owner", "用户头像必须给出 Owner")
	}
	if len([]rune(p.Name)) > 255 || strings.ContainsAny(p.Name, `/\`) || strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
		return invalidParams("name", `文件名最长 255 个字符，不能含 / \ 和控制字符`)
	}
	if p.Concurrency < 0 {
		return invalidParams("concurrency", "Concurrency 不能为负数")
	}
	return nil
}

func (s *MediaService) cancelQuietly(ctx context.Context, fileID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	if err := s.CancelUpload(WithMaxAttempts(cctx, 1), fileID); err != nil {
		s.t.log.Debug("取消上传失败，已忽略（到期后服务端会清理）", "file_id", fileID, "error", err)
	}
}

type uploader struct {
	s           *MediaService
	r           io.ReaderAt
	size        int64
	fileID      string
	concurrency int
	progress    *progress
}

func (s *MediaService) uploader(r io.ReaderAt, size int64, fileID string, p UploadParams) *uploader {
	concurrency := p.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	return &uploader{s: s, r: r, size: size, fileID: fileID, concurrency: concurrency, progress: &progress{total: size, fn: p.OnProgress}}
}

type progress struct {
	mu    sync.Mutex
	done  int64
	total int64
	fn    func(uploaded, total int64)
}

func (p *progress) add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done = min(p.total, p.done+n)
	if p.fn != nil {
		p.fn(p.done, p.total)
	}
}

func (u *uploader) run(ctx context.Context, session *UploadSession) (*File, error) {
	if session.UploadMethod == "single" || session.Upload != nil {
		if session.Upload == nil {
			e := newError(CodeInvalidResponse, "创建上传的结果缺少上传地址")
			return nil, u.s.t.localError("media.upload", e)
		}
		if err := u.put(ctx, "media.upload", session.Upload.URL, session.Upload.Headers, 0, u.size); err != nil {
			return nil, err
		}
		u.progress.add(u.size)
		return u.complete(ctx, partSize, false)
	}
	size := int64(partSize)
	if session.PartSize != nil && *session.PartSize > 0 {
		size = *session.PartSize
	}
	numbers := make([]int, 0, session.PartCount)
	for i := 1; i <= int(session.PartCount); i++ {
		numbers = append(numbers, i)
	}
	if len(numbers) == 0 {
		numbers = partNumbers(u.size, size)
	}
	if err := u.uploadParts(ctx, size, numbers); err != nil {
		return nil, err
	}
	return u.complete(ctx, size, true)
}

func partNumbers(size, part int64) []int {
	n := int((size + part - 1) / part)
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func (u *uploader) partRange(number int, part int64) (int64, int64) {
	offset := int64(number-1) * part
	return offset, min(part, u.size-offset)
}

func (u *uploader) bytesOf(numbers []int, part int64) int64 {
	var total int64
	for _, n := range numbers {
		_, length := u.partRange(n, part)
		total += length
	}
	return total
}

// missingParts 查询已上传的分片，返回缺少的和大小不对的分片号。
func (u *uploader) missingParts(ctx context.Context, part int64) ([]int, error) {
	uploaded, err := u.s.UploadedParts(ctx, u.fileID)
	if err != nil {
		return nil, err
	}
	sizes := make(map[int]int64, len(uploaded))
	for _, p := range uploaded {
		sizes[p.PartNumber] = p.Size
	}
	var missing []int
	for _, n := range partNumbers(u.size, part) {
		_, length := u.partRange(n, part)
		if sizes[n] != length {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// uploadParts 并发上传分片：用到时再索取地址（每次最多 100 个），地址过期（403）时重新索取这个分片的地址再传。
func (u *uploader) uploadParts(ctx context.Context, part int64, numbers []int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		urls     = map[int]PartURL{}
		firstErr error
		wg       sync.WaitGroup
	)
	position := map[int]int{}
	for i, n := range numbers {
		position[n] = i
	}
	batch := min(presignBatch, u.concurrency*2)
	presign := func(number int) (PartURL, error) {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := urls[number]; ok {
			return p, nil
		}
		// 这一个和后面还没有地址的，凑成一批
		wanted := []int{number}
		for _, n := range numbers[position[number]+1:] {
			if len(wanted) >= batch {
				break
			}
			if _, ok := urls[n]; !ok {
				wanted = append(wanted, n)
			}
		}
		parts, err := u.s.PartURLs(ctx, u.fileID, wanted)
		if err != nil {
			return PartURL{}, err
		}
		for _, p := range parts {
			urls[p.PartNumber] = p
		}
		p, ok := urls[number]
		if !ok {
			e := newError(CodeInvalidResponse, "索取分片地址的结果中缺少分片 "+strconv.Itoa(number))
			return PartURL{}, u.s.t.localError("media.part_urls", e)
		}
		return p, nil
	}
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	slots := make(chan struct{}, u.concurrency)
	for _, number := range numbers {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(number int) {
			defer wg.Done()
			defer func() { <-slots }()
			offset, length := u.partRange(number, part)
			for tries := 0; ; tries++ {
				p, err := presign(number)
				if err != nil {
					fail(err)
					return
				}
				err = u.put(ctx, "media.upload_part", p.URL, p.Headers, offset, length)
				if errors.Is(err, errExpired) && tries < 2 {
					mu.Lock()
					delete(urls, number) // 地址已过期：重新索取这个分片的地址
					mu.Unlock()
					continue
				}
				if errors.Is(err, errExpired) {
					err = u.s.t.localError("media.upload_part", newError(CodeNetworkError, "分片 "+strconv.Itoa(number)+" 的上传地址反复过期"))
				}
				if err != nil {
					fail(err)
					return
				}
				u.progress.add(length)
				return
			}
		}(number)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr // 最初的错误（因为它而取消的其他分片的错误不返回）
	}
	if ctx.Err() != nil {
		return u.s.t.localError("media.upload", ctxError(ctx))
	}
	return nil
}

// put 上传到对象存储：带上返回的请求头（Content-Length 在签名之内，按给出的值发送），不带 Authorization 和 Content-Type。
// 网络错误、5xx、429 按 1、2、4 秒重试；403 视为地址过期（errExpired）。
func (u *uploader) put(ctx context.Context, op, url string, headers map[string]string, offset, length int64) error {
	t := u.s.t
	for attempt := 0; ; attempt++ {
		err := func() error {
			actx, cancel := context.WithTimeout(ctx, max(t.timeout, objectPutTimeout))
			defer cancel()
			req, err := http.NewRequestWithContext(actx, http.MethodPut, url, nil)
			if err != nil {
				return t.localError(op, invalidParams("url", "上传地址不合法："+err.Error()))
			}
			req.Body = io.NopCloser(io.NewSectionReader(u.r, offset, length))
			req.ContentLength = length
			for key, value := range headers {
				if strings.EqualFold(key, "Content-Length") {
					if n, perr := strconv.ParseInt(value, 10, 64); perr == nil && n != length {
						e := newError(CodeInvalidResponse, fmt.Sprintf("上传地址要求的 Content-Length（%d）与内容的大小（%d）不同", n, length))
						return t.localError(op, e)
					}
					continue
				}
				req.Header.Set(key, value)
			}
			resp, err := t.http.Do(req)
			if err != nil {
				f := classify(ctx, actx, err, true)
				if f.final {
					return t.localError(op, f.err)
				}
				return &retryable{t.localError(op, f.err)}
			}
			body, _ := readLimited(resp.Body, maxBodyPrefix)
			resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return nil
			case resp.StatusCode == http.StatusForbidden:
				return errExpired
			}
			e := newError(CodeNetworkError, fmt.Sprintf("上传到对象存储失败（HTTP %d）", resp.StatusCode))
			e.StatusCode = resp.StatusCode
			e.Details = map[string]any{"body_prefix": bodyPrefix(body)}
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				return &retryable{t.localError(op, e)}
			}
			return t.localError(op, e)
		}()
		var r *retryable
		if !errors.As(err, &r) {
			return err
		}
		if attempt >= len(putDelays) {
			return r.err
		}
		if sleepCtx(ctx, putDelays[attempt]) != nil {
			return t.localError(op, ctxError(ctx))
		}
	}
}

type retryable struct{ err *Error }

func (r *retryable) Error() string { return r.err.Error() }

// complete 完成上传，按《1.功能概述文档》第 10 节的表处理各种结果。
func (u *uploader) complete(ctx context.Context, part int64, multipart bool) (*File, error) {
	started := time.Now()
	completing, changed := 0, 0
	reuploaded := false
	for {
		file, err := u.s.CompleteUpload(ctx, u.fileID)
		if err == nil {
			return file, nil
		}
		var e *Error
		if !errors.As(err, &e) {
			return nil, err
		}
		switch {
		case e.Code == CodeVersionConflict && e.Reason() == "upload_completing":
			// 另一个完成请求正在处理（如前一次尝试的连接被服务端的写超时关闭）：等待后重试，总计不超过 3 分钟
			delay := completingDelays[min(completing, len(completingDelays)-1)]
			if time.Since(started)+delay > completingTotal {
				return nil, err
			}
			completing++
			if sleepCtx(ctx, delay) != nil {
				return nil, u.s.t.localError("media.complete_upload", ctxError(ctx))
			}
		case e.Code == CodeVersionConflict && e.Reason() == "upload_changed" && changed < maxChangedRetries:
			changed++ // 核对期间内容又被重新上传：立即重新完成
		case e.Code == CodeInvalidArgument && e.Reason() == "upload_incomplete" && multipart && !reuploaded:
			reuploaded = true
			missing, merr := u.missingParts(ctx, part)
			if merr != nil {
				return nil, merr
			}
			if len(missing) > 0 {
				u.s.t.log.Warn("完成上传时分片不全，补传后再完成", "file_id", u.fileID, "parts", len(missing))
				if perr := u.uploadParts(ctx, part, missing); perr != nil {
					return nil, perr
				}
			}
		default:
			return nil, err
		}
	}
}
