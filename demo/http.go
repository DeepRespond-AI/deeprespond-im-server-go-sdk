package main

// 注册、登录、上传和浏览器 Cookie 接口。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	drim "github.com/DeepRespond-AI/deeprespond-im-server-go-sdk"
)

const cookieName = "demo_sid"

var direct = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 15 * time.Second}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// sessionOf 取 cookie 对应的浏览器会话；未登录或已失效时为 nil。
func (st *AppState) sessionOf(r *http.Request) *BrowserSession {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions[c.Value]
	if s == nil || s.dead {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// HTTP 接口（注册 / 登录 / 登出 / 上传 / 媒体代理）

func (st *AppState) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	username := strings.TrimSpace(param(body, "username"))
	password := param(body, "password")
	nickname := strings.TrimSpace(param(body, "nickname"))
	if username == "" || password == "" {
		jsonError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}
	if len(password) < 6 {
		jsonError(w, http.StatusBadRequest, "密码至少 6 位")
		return
	}
	_, err := st.client.Users.Create(r.Context(), drim.CreateUserParams{
		Username: username, Password: password, Nickname: nickname,
	})
	if err != nil {
		if drim.IsCode(err, drim.CodeAlreadyExists) {
			jsonError(w, http.StatusConflict, "用户名已被注册")
			return
		}
		jsonError(w, http.StatusBadGateway, errText(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (st *AppState) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	username := strings.TrimSpace(param(body, "username"))
	password := param(body, "password")
	if username == "" || password == "" {
		jsonError(w, http.StatusBadRequest, "参数不完整")
		return
	}
	auth, err := clientLogin(st.orgName, st.appName, st.apiURL, username, password)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "用户名或密码错误，或账号已被禁用")
		return
	}
	// 同一浏览器重复登录：清理旧会话
	if c, err := r.Cookie(cookieName); err == nil {
		if old := st.sessionBySID(c.Value); old != nil {
			if err := st.dropSession(old, true); err != nil {
				_ = st.client.Users.Kick(r.Context(), username, strOf(auth["session_id"]))
				jsonError(w, 502, errText(err))
				return
			}
		}
	}
	st.mu.Lock()
	session := &BrowserSession{
		SID:         randHex(18),
		Username:    username,
		IMSessionID: strOf(auth["session_id"]),
		device:      newDevice(st.apiURL, auth),
	}
	st.sessions[session.SID] = session
	ctx := st.contextOf(username)
	ctx.Sessions = append(ctx.Sessions, session)
	if user, ok := auth["user"].(map[string]any); ok && user != nil {
		ctx.Me = user
	}
	me := copyJSON(ctx.Me)
	st.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: session.SID, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "me": me})
}

func (st *AppState) sessionBySID(sid string) *BrowserSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessions[sid]
}

func (st *AppState) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if s := st.sessionBySID(c.Value); s != nil {
			if err := st.dropSession(s, true); err != nil {
				jsonError(w, 502, errText(err))
				return
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (st *AppState) handleUpload(w http.ResponseWriter, r *http.Request) {
	session := st.sessionOf(r)
	if session == nil {
		jsonError(w, http.StatusUnauthorized, "未登录")
		return
	}
	if _, err := st.authenticate(session); err != nil {
		jsonError(w, 401, errText(err))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 40<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, http.StatusBadRequest, "上传内容过大或格式非法")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "缺少上传内容")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		jsonError(w, http.StatusBadRequest, "上传内容为空")
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	kind := "file"
	switch {
	case strings.HasPrefix(contentType, "image/"):
		kind = "image"
	case strings.HasPrefix(contentType, "video/"):
		kind = "video"
	}
	name := header.Filename
	if name == "" {
		name = "file"
	}
	info, err := st.client.Media.Upload(r.Context(), bytes.NewReader(data), int64(len(data)), drim.UploadParams{
		Purpose: "attachment", Kind: kind, Name: name, ContentType: contentType, Owner: session.Username,
	})
	if err != nil {
		jsonError(w, http.StatusBadGateway, errText(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file_info": rawMap(info.Raw)})
}

// handler 组合路由，测试和 CLI 使用同一个 HTTP 入口。
func (st *AppState) handler() (http.Handler, error) {
	callbackHandler, err := st.buildCallbackHandler()
	if err != nil {
		return nil, err
	}

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("POST /im/callback", callbackHandler)
	mux.HandleFunc("POST /api/register", st.handleRegister)
	mux.HandleFunc("POST /api/login", st.handleLogin)
	mux.HandleFunc("POST /api/logout", st.handleLogout)
	mux.HandleFunc("POST /api/upload", st.handleUpload)
	mux.HandleFunc("GET /api/media", st.handleMedia)
	mux.HandleFunc("GET /ws", st.handleWS)
	mux.Handle("GET /", http.FileServerFS(webRoot))

	return mux, nil
}
