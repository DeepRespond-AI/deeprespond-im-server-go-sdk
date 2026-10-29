package main

// 附件只内联验证过的光栅图片；文件名和 ct 参数均不作为类型依据。

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"os"
)

func (st *AppState) handleMedia(w http.ResponseWriter, r *http.Request) {
	s := st.sessionOf(r)
	if s == nil {
		jsonError(w, 401, "未登录")
		return
	}
	if _, err := st.authenticate(s); err != nil {
		jsonError(w, 401, errText(err))
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("u"))
	if err != nil || len(raw) == 0 {
		jsonError(w, 400, "u 编码非法")
		return
	}
	// 临时文件避免大附件常驻内存，也确保下载失败时仍可返回明确的错误。
	file, err := os.CreateTemp("", "im-demo-media-*")
	if err != nil {
		jsonError(w, 500, "无法创建下载缓存")
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = st.client.Media.Download(r.Context(), string(raw), file); err != nil {
		jsonError(w, 502, errText(err))
		return
	}
	head := make([]byte, 512)
	n, _ := file.ReadAt(head, 0)
	writeMediaHeaders(w, head[:n])
	_, _ = file.Seek(0, io.SeekStart)
	_, _ = io.Copy(w, file)
}

func writeMediaHeaders(w http.ResponseWriter, head []byte) {
	ct := "application/octet-stream"
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		ct = "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		ct = "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		ct = "image/gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		ct = "image/webp"
	}
	w.Header().Set("Content-Type", ct)
	disposition := `attachment; filename="download"`
	if ct != "application/octet-stream" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
}
