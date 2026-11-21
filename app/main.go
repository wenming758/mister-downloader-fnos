package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	appID           = "mister-downloader"
	appDisplayName  = "Mister 下载器"
	appVersion      = "1.0.2"
	upstreamVersion = "0.5.2"
)

//go:embed web
var webFS embed.FS

type App struct {
	auth     *Auth
	mgr      *Manager
	upd      *Updater
	dataRoot string
	appDir   string
	web      fs.FS
}

func main() {
	log.SetFlags(log.LstdFlags)
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("无法定位程序路径: %v", err)
	}
	appDir := os.Getenv("APP_DIR")
	if appDir == "" {
		appDir = filepath.Dir(exe)
	}
	dataRoot := os.Getenv("DATA_DIR")
	if dataRoot == "" {
		dataRoot = filepath.Join(appDir, "data")
	}
	for _, d := range []string{dataRoot, filepath.Join(dataRoot, "logs"), filepath.Join(dataRoot, "mdm")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			log.Fatalf("无法创建数据目录 %s: %v", d, err)
		}
	}
	logPath := filepath.Join(dataRoot, "logs", "server.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		defer f.Close()
	}

	auth, err := loadAuth(filepath.Join(dataRoot, "config.json"))
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}

	dlFn := func() string {
		if d := auth.DownloadDir(); d != "" {
			return d
		}
		return filepath.Join(dataRoot, "downloads")
	}
	mgr := NewManager(appDir, dataRoot, mdmPort, dlFn)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("读取内置页面失败: %v", err)
	}
	a := &App{auth: auth, mgr: mgr, upd: NewUpdater(), dataRoot: dataRoot, appDir: appDir, web: sub}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start()
	go mgr.Watchdog(ctx)

	handler := a.routes()
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8089"
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 15 * time.Second}

	if sock := os.Getenv("APP_SOCKET"); sock != "" {
		_ = os.Remove(sock)
		if ln, err := net.Listen("unix", sock); err == nil {
			_ = os.Chmod(sock, 0660)
			go func() {
				log.Printf("网关套接字监听: %s", sock)
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("网关套接字停止: %v", err)
				}
			}()
		} else {
			log.Printf("网关套接字创建失败 %s: %v", sock, err)
		}
	}

	go func() {
		log.Printf("%s v%s 启动，监听 %s", appDisplayName, appVersion, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP 服务停止: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在停止服务…")
	cancel()
	mgr.Stop()
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = srv.Shutdown(shutdownCtx)
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/fnapi/", a.handleAPI)
	mux.HandleFunc("/assets/", a.handleAsset)
	mux.HandleFunc("/panel", a.handlePanelBare)
	mux.HandleFunc("/panel/", a.handlePanel)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", a.handleShell)
	prefix := "/app/" + appID
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic: %v", err)
				http.Error(w, "内部错误", http.StatusInternalServerError)
			}
		}()
		if strings.HasPrefix(r.URL.Path, prefix) {
			if r.URL.Path == prefix {
				http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
				return
			}
			p := strings.TrimPrefix(r.URL.Path, prefix)
			if p == "" {
				p = "/"
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = p
			mux.ServeHTTP(w, r2)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) commonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Cache-Control", "no-store")
}

func (a *App) handleShell(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	raw, err := fs.ReadFile(a.web, "index.html")
	if err != nil {
		http.Error(w, "页面缺失", http.StatusInternalServerError)
		return
	}
	a.commonHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'")
	_, _ = w.Write(raw)
}

func (a *App) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	raw, err := fs.ReadFile(a.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(raw)
}

func (a *App) handlePanelBare(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "../panel/", http.StatusTemporaryRedirect)
}

func (a *App) handlePanel(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/panel")
	if path == "" {
		path = "/"
	}
	// 上游页面调用接口时使用 credentials: 'omit'（不携带 Cookie），
	// 因此面板接口额外接受应用自己的下载引擎令牌。
	if strings.HasPrefix(path, "/api/") {
		_, hasSession := a.auth.Session(r)
		if !hasSession {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" || tok != a.mgr.Token() {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not paired", "code": "not-paired"})
				return
			}
		}
		if path == "/api/forget" {
			// 解绑设备会中断自动化配对，交回应用外壳管理。
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forget is managed by the fnOS wrapper", "code": "forbidden"})
			return
		}
	} else if _, ok := a.auth.Session(r); !ok {
		http.Redirect(w, r, "../", http.StatusTemporaryRedirect)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	a.mgr.proxy.ServeHTTP(w, r2)
}

func (a *App) requireUser(w http.ResponseWriter, r *http.Request, admin bool) (session, bool) {
	s, ok := a.auth.Session(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "请先登录"})
		return session{}, false
	}
	if admin && s.Role != roleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "需要管理员权限"})
		return session{}, false
	}
	return s, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error string `json:"error"`
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errBody{Error: msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "请求格式不正确")
		return false
	}
	return true
}

func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Header.Get("X-Requested-With") != "mdm-fnos" {
		fail(w, http.StatusForbidden, "请求来源无效")
		return
	}
	a.commonHeaders(w)
	switch r.URL.Path {
	case "/fnapi/me":
		a.apiMe(w, r)
	case "/fnapi/login":
		a.apiLogin(w, r)
	case "/fnapi/logout":
		a.apiLogout(w, r)
	case "/fnapi/init":
		a.apiInit(w, r)
	case "/fnapi/status":
		a.apiStatus(w, r)
	case "/fnapi/settings":
		a.apiSettings(w, r)
	case "/fnapi/users":
		a.apiUsers(w, r)
	case "/fnapi/update":
		a.apiUpdate(w, r)
	case "/fnapi/logs":
		a.apiLogs(w, r)
	case "/fnapi/restart":
		a.apiRestart(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) apiMe(w http.ResponseWriter, r *http.Request) {
	s, ok := a.auth.Session(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"initialized": a.auth.Initialized(),
			"version":     appVersion,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":           map[string]string{"name": s.Name, "role": s.Role},
		"initialized":    true,
		"version":        appVersion,
		"upstream":       upstreamVersion,
		"download_dir":   a.mgr.dirFn(),
		"data_dir":       a.dataRoot,
		"mdm_token":      a.mgr.Token(),
		"gitee_repo":     giteeRepo,
		"gitee_releases": giteeReleasesPage,
	})
}

func (a *App) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *App) apiInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if a.auth.Initialized() {
		fail(w, http.StatusConflict, "应用已完成初始化")
		return
	}
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validName(in.Name) {
		fail(w, http.StatusBadRequest, "用户名可用 1-32 个字符，不能包含斜杠")
		return
	}
	if in.Password == "" {
		fail(w, http.StatusBadRequest, "密码不能为空")
		return
	}
	if len(in.Password) > 512 {
		fail(w, http.StatusBadRequest, "密码过长")
		return
	}
	if err := a.auth.CreateInitial(in.Name, in.Password); err != nil {
		fail(w, http.StatusConflict, "初始化失败")
		return
	}
	tok := a.auth.NewSession(in.Name, roleAdmin)
	a.setSessionCookie(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]string{"name": in.Name, "role": roleAdmin}, "session": tok})
}

func (a *App) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if !a.auth.Allow(clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	u, ok := a.auth.Authenticate(strings.TrimSpace(in.Name), in.Password)
	if !ok {
		fail(w, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}
	tok := a.auth.NewSession(u.Name, u.Role)
	a.setSessionCookie(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]string{"name": u.Name, "role": u.Role}, "session": tok})
}

func (a *App) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.auth.DropSession(c.Value)
	}
	if h := r.Header.Get("X-MDM-Session"); h != "" {
		a.auth.DropSession(h)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	st := a.mgr.Summary(r.Context())
	writeJSON(w, http.StatusOK, st)
}

func (a *App) apiSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, true); !ok {
		return
	}
	var in struct {
		DownloadDir string `json:"download_dir"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	dir := cleanDir(in.DownloadDir)
	if dir == "" {
		fail(w, http.StatusBadRequest, "请输入绝对路径，例如 /vol1/1000/downloads")
		return
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("无法创建下载目录：%v", err))
		return
	}
	probe := filepath.Join(dir, ".mdm-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("下载目录不可写：%v", err))
		return
	}
	_ = os.Remove(probe)
	if err := a.auth.SetDownloadDir(dir); err != nil {
		fail(w, http.StatusInternalServerError, "保存设置失败")
		return
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.mgr.Restart()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download_dir": dir, "message": "已保存，下载引擎正在重启"})
}

func (a *App) apiUsers(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireUser(w, r, true)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"users": a.auth.Users(), "me": s.Name})
	case http.MethodPost:
		var in struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if !validName(in.Name) {
			fail(w, http.StatusBadRequest, "用户名可用 1-32 个字符，不能包含斜杠")
			return
		}
		if _, exists := a.auth.User(in.Name); !exists && in.Password == "" {
			fail(w, http.StatusBadRequest, "新用户必须设置密码")
			return
		}
		if len(in.Password) > 512 {
			fail(w, http.StatusBadRequest, "密码过长")
			return
		}
		if err := a.auth.AddUser(in.Name, in.Password, in.Role); err != nil {
			if err == errLastAdmin {
				fail(w, http.StatusBadRequest, "至少需要保留一个管理员")
				return
			}
			fail(w, http.StatusInternalServerError, "保存用户失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			fail(w, http.StatusBadRequest, "缺少用户名")
			return
		}
		if name == s.Name {
			fail(w, http.StatusBadRequest, "不能删除当前登录的账户")
			return
		}
		if err := a.auth.DeleteUser(name); err != nil {
			if err == errLastAdmin {
				fail(w, http.StatusBadRequest, "至少需要保留一个管理员")
				return
			}
			fail(w, http.StatusNotFound, "用户不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

func (a *App) apiUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, http.StatusOK, a.upd.Check(r.Context(), force))
}

func (a *App) apiLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r, true); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"log":  a.mgr.TailLog(16000),
		"path": a.mgr.logPath,
	})
}

func (a *App) apiRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, true); !ok {
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		a.mgr.Restart()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "下载引擎正在重启"})
}
