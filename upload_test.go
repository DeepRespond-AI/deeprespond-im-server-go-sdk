package drim_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
	"github.com/DeepRespond-AI/deeprespond-im-server-go-sdk/drimtest"
)

// objectStore 是假的对象存储：PUT /parts/{n} 保存分片，GET /files/{id} 下载。
type objectStore struct {
	srv       *httptest.Server
	mu        sync.Mutex
	parts     map[int][]byte
	single    []byte
	expireOne map[int]bool // 这些分片第一次返回 403（地址过期）
	headers   []http.Header
	slow      time.Duration
	download  []byte
	expiredDL atomic.Int32
}

func newObjectStore(t *testing.T) *objectStore {
	o := &objectStore{parts: map[int][]byte{}, expireOne: map[int]bool{}}
	o.srv = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *objectStore) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if o.expiredDL.Add(-1) >= 0 {
			w.WriteHeader(403)
			w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
			return
		}
		w.Write(o.download)
		return
	}
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.headers = append(o.headers, r.Header.Clone())
	if o.slow > 0 {
		o.mu.Unlock()
		time.Sleep(o.slow)
		o.mu.Lock()
	}
	if r.URL.Path == "/single" {
		o.single = body
		return
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/parts/"))
	if o.expireOne[n] && r.URL.Query().Get("v") == "1" {
		w.WriteHeader(403)
		w.Write([]byte("<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>"))
		return
	}
	if int64(len(body)) != r.ContentLength {
		w.WriteHeader(400)
		return
	}
	o.parts[n] = body
}

func (o *objectStore) assembled(count int) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []byte
	for i := 1; i <= count; i++ {
		out = append(out, o.parts[i]...)
	}
	return out
}

const testPart = 64 * 1024

// fakeMedia 在假的 OpenAPI 上实现上传的几个接口（分片为 64 KB，便于测试）。
func fakeMedia(t *testing.T, srv *drimtest.Server, store *objectStore, size int) *atomic.Int32 {
	count := (size + testPart - 1) / testPart
	var presigned sync.Map
	var completes atomic.Int32
	srv.Handle("media.create_upload", func(w http.ResponseWriter, r *http.Request) {
		if size <= testPart {
			fmt.Fprintf(w, `{"file_id":"f1","upload_method":"single","upload":{"url":"%s/single","method":"PUT","headers":{"Content-Length":"%d"}},"part_size":null,"part_count":0}`, store.srv.URL, size)
			return
		}
		fmt.Fprintf(w, `{"file_id":"f1","upload_method":"multipart","upload":null,"part_size":%d,"part_count":%d}`, testPart, count)
	})
	srv.Handle("media.part_urls", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PartNumbers []int `json:"part_numbers"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var parts []string
		for _, n := range req.PartNumbers {
			v, _ := presigned.LoadOrStore(n, new(atomic.Int32))
			version := v.(*atomic.Int32).Add(1)
			length := testPart
			if n == count {
				length = size - (count-1)*testPart
			}
			parts = append(parts, fmt.Sprintf(`{"part_number":%d,"url":"%s/parts/%d?v=%d","method":"PUT","headers":{"Content-Length":"%d"}}`,
				n, store.srv.URL, n, version, length))
		}
		fmt.Fprintf(w, `{"parts":[%s]}`, strings.Join(parts, ","))
	})
	srv.Handle("media.uploaded_parts", func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		defer store.mu.Unlock()
		var parts []string
		for n, data := range store.parts {
			parts = append(parts, fmt.Sprintf(`{"part_number":%d,"size":%d}`, n, len(data)))
		}
		fmt.Fprintf(w, `{"parts":[%s]}`, strings.Join(parts, ","))
	})
	srv.Handle("media.complete_upload", func(w http.ResponseWriter, r *http.Request) {
		completes.Add(1)
		store.mu.Lock()
		got := len(store.parts)
		store.mu.Unlock()
		if size > testPart && got < count {
			w.WriteHeader(400)
			w.Write(drimtest.ErrorBody("invalid_argument", "分片不全", map[string]any{"reason": "upload_incomplete"}))
			return
		}
		w.Write([]byte(`{"file_id":"f1","status":"active","url":"https://im.example.com/media/f1","size":` + strconv.Itoa(size) + `}`))
	})
	return &completes
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestUploadMultipart(t *testing.T) { // S-21、S-54
	srv := newServer(t)
	store := newObjectStore(t)
	store.expireOne[2] = true
	data := randomBytes(5*testPart + 123)
	fakeMedia(t, srv, store, len(data))
	client := newClient(t, srv)
	var progress []int64
	file, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{
		Purpose: "attachment", Kind: "file", Name: "a.bin", Concurrency: 3,
		OnProgress: func(done, total int64) { progress = append(progress, done) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if file.URL != "https://im.example.com/media/f1" || !bytes.Equal(store.assembled(6), data) {
		t.Fatal("内容不对")
	}
	if progress[len(progress)-1] != int64(len(data)) || len(progress) != 6 {
		t.Fatalf("进度：%v", progress)
	}
	for _, h := range store.headers {
		if h.Get("Authorization") != "" || h.Get("Content-Type") != "" {
			t.Fatalf("上传到对象存储不带 Authorization 和 Content-Type：%v", h)
		}
	}
	if body := srv.Calls("media.create_upload")[0].JSON(); body["size"] != float64(len(data)) || body["name"] != "a.bin" {
		t.Fatal(body)
	}
	if n := len(srv.Calls("media.part_urls")); n < 2 {
		t.Fatalf("地址过期后重新索取：%d", n)
	}
}

func TestUploadSingleAndChecks(t *testing.T) {
	srv := newServer(t)
	store := newObjectStore(t)
	data := randomBytes(1000)
	fakeMedia(t, srv, store, len(data))
	client := newClient(t, srv)
	file, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "image"})
	if err != nil || file.FileID != "f1" || !bytes.Equal(store.single, data) {
		t.Fatal(file, err)
	}
	bad := []drim.UploadParams{
		{Purpose: "user_avatar", Kind: "video", Owner: "a"},
		{Purpose: "attachment", Kind: "file"},
		{Purpose: "group_file", Kind: "file", Name: "a"},
		{Purpose: "attachment", Kind: "file", Name: "a/b"},
		{Purpose: "nope", Kind: "file"},
	}
	for _, p := range bad {
		_, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), p)
		if e := asError(t, err); e.Code != drim.CodeInvalidParams || e.Op != "media.upload" {
			t.Fatalf("%+v：%+v", p, e)
		}
	}
	_, err = client.Media.Upload(context.Background(), bytes.NewReader(nil), 6<<20, drim.UploadParams{Purpose: "user_avatar", Kind: "image", Owner: "a"})
	if e := asError(t, err); e.Code != drim.CodeInvalidParams {
		t.Fatal(e)
	}
}

func TestUploadIncompleteReupload(t *testing.T) { // S-55
	srv := newServer(t)
	store := newObjectStore(t)
	data := randomBytes(3*testPart + 1)
	completes := fakeMedia(t, srv, store, len(data))
	var lost atomic.Bool
	// 第一次完成时“丢失”一个分片
	srv.Handle("media.complete_upload", func(w http.ResponseWriter, r *http.Request) {
		completes.Add(1)
		if !lost.Swap(true) {
			store.mu.Lock()
			delete(store.parts, 2)
			store.mu.Unlock()
			w.WriteHeader(400)
			w.Write(drimtest.ErrorBody("invalid_argument", "分片不全", map[string]any{"reason": "upload_incomplete"}))
			return
		}
		w.Write([]byte(`{"file_id":"f1","status":"active","url":"u"}`))
	}, drimtest.Times(2))
	client := newClient(t, srv)
	if _, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "file", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.assembled(4), data) || completes.Load() != 2 {
		t.Fatalf("补传后再完成：%d", completes.Load())
	}
	// 再次缺失时返回错误（只重来 1 次）
	srv.Fail("media.complete_upload", "invalid_argument", drimtest.FailOptions{Details: map[string]any{"reason": "upload_incomplete"}, Times: 2})
	_, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "file", Name: "x"})
	if !drim.IsReason(err, "invalid_argument", "upload_incomplete") {
		t.Fatal(err)
	}
}

func TestUploadCanceled(t *testing.T) { // S-56
	srv := newServer(t)
	store := newObjectStore(t)
	store.slow = 300 * time.Millisecond
	data := randomBytes(4 * testPart)
	fakeMedia(t, srv, store, len(data))
	client := newClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := client.Media.Upload(ctx, bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "file", Name: "x", Concurrency: 1})
	if e := asError(t, err); e.Code != drim.CodeCanceled {
		t.Fatal(e)
	}
	if n := len(srv.Calls("media.cancel_upload")); n != 1 {
		t.Fatalf("应当取消这次上传：%d", n)
	}
}

func TestCompleteResults(t *testing.T) { // S-57
	srv := newServer(t)
	store := newObjectStore(t)
	data := randomBytes(100)
	fakeMedia(t, srv, store, len(data))
	client := newClient(t, srv)
	upload := func() error {
		_, err := client.Media.Upload(context.Background(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{Purpose: "attachment", Kind: "image"})
		return err
	}
	for _, reason := range []string{"size_mismatch", "unsupported_type"} {
		srv.Fail("media.complete_upload", "invalid_argument", drimtest.FailOptions{Details: map[string]any{"reason": reason}})
		before := len(srv.Calls("media.complete_upload"))
		if err := upload(); !drim.IsReason(err, "invalid_argument", reason) {
			t.Fatal(err)
		}
		if len(srv.Calls("media.complete_upload"))-before != 1 {
			t.Fatal("不重来")
		}
	}
	srv.Fail("media.complete_upload", "rate_limited", drimtest.FailOptions{RetryAfter: "1", Details: map[string]any{"reason": "image_busy"}})
	if err := upload(); err != nil {
		t.Fatal(err)
	}
	srv.Fail("media.complete_upload", "version_conflict", drimtest.FailOptions{Details: map[string]any{"reason": "upload_completing"}, Times: 2})
	if err := upload(); err != nil {
		t.Fatal(err)
	}
	srv.Fail("media.complete_upload", "version_conflict", drimtest.FailOptions{Details: map[string]any{"reason": "upload_changed"}})
	if err := upload(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeUpload(t *testing.T) {
	srv := newServer(t)
	store := newObjectStore(t)
	data := randomBytes(3*testPart + 10)
	fakeMedia(t, srv, store, len(data))
	store.parts[1] = data[:testPart]
	store.parts[2] = data[testPart : testPart+100] // 大小不对
	client := newClient(t, srv)
	defer drim.SetResumePartSize(testPart)()
	if _, err := client.Media.ResumeUpload(context.Background(), "f1", bytes.NewReader(data), int64(len(data)), drim.UploadParams{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.assembled(4), data) {
		t.Fatal("续传后内容不对")
	}
	var urls []float64
	for _, call := range srv.Calls("media.part_urls") {
		for _, n := range call.JSON()["part_numbers"].([]any) {
			urls = append(urls, n.(float64))
		}
	}
	sort.Float64s(urls)
	if fmt.Sprint(urls) != "[2 3 4]" {
		t.Fatalf("只补传缺少的和大小不对的：%v", urls)
	}
}

func TestDownload(t *testing.T) {
	srv := newServer(t)
	store := newObjectStore(t)
	store.download = []byte("文件内容")
	store.expiredDL.Store(1)
	srv.Handle("media.download_urls", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"items":[{"url":"https://im.example.com/media/f1","download_url":"%s/files/f1?sig=x","expires_at":"2026-10-06T10:00:00.000Z"}]}`, store.srv.URL)
	})
	client := newClient(t, srv)
	var buf bytes.Buffer
	n, err := client.Media.Download(context.Background(), "https://im.example.com/media/f1", &buf)
	if err != nil || n != int64(len("文件内容")) || buf.String() != "文件内容" {
		t.Fatal(n, err)
	}
	if len(srv.Calls("media.download_urls")) != 2 {
		t.Fatal("下载地址过期时重新换取一次")
	}
	srv.Respond("media.download_urls", 200, `{"items":[{"url":"u","code":"file_expired","message":"已过期"}]}`)
	_, err = client.Media.Download(context.Background(), "u", io.Discard)
	if e := asError(t, err); e.Code != drim.CodeFileExpired || e.Op != "media.download" {
		t.Fatal(e)
	}
}
