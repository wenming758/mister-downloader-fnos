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
	"sync"
	"syscall"
	"time"
)

const (
	appID           = "mister-downloader"
	appDisplayName  = "Mister 下载器"
	appVersion      = "1.0.10"
	upstreamVersion = "0.5.2"
)

//go:embed web
var webFS embed.FS

type App struct {
	auth     *Auth
	mgr      *Manager
	upd      *Updater
	engChk   *EngineChecker
	self     *SelfUpdater
	eng      *EngineUpdater
	hist     *History
	dataRoot string
	appDir   string
	web      fs.FS
	dirMu    sync.Mutex
	pendMu   sync.Mutex
	pending  map[string]*PendingMagnet
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
	a := &App{auth: auth, mgr: mgr, upd: NewUpdater(), engChk: NewEngineChecker(), self: NewSelfUpdater(appDir, dataRoot), hist: NewHistory(dataRoot), dataRoot: dataRoot, appDir: appDir, web: sub, pending: make(map[string]*PendingMagnet)}
	a.eng = NewEngineUpdater(appDir, dataRoot, mgr)
	a.self.beforeRestart = func() {
		a.captureRunningForUpdate()
		a.mgr.Stop()
	}
	a.self.onAbort = func() {
		a.mgr.Start()
		go a.resumeAfterUpdate(context.Background())
	}
	a.eng.onReady = func() { a.resumeAfterUpdate(context.Background()) }
	a.self.Init()
	a.eng.Init()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start()
	go mgr.Watchdog(ctx)
	go a.resumeAfterUpdate(ctx)

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
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		w = rec
		defer func() {
			p := r.URL.Path
			if !strings.HasPrefix(p, "/panel") && !strings.HasPrefix(p, "/fnapi/") {
				return
			}
			code := rec.statusCode()
			took := time.Since(start)
			// 正常轮询不刷日志；失败、慢请求和面板首页加载都记下来，方便在设备上排查。
			if code >= 400 || took > 3*time.Second || p == "/panel/" || strings.HasSuffix(p, "/panel/") || p == "/panel" {
				log.Printf("[req] %s %s -> %d (%s)", r.Method, p, code, took.Round(time.Millisecond))
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

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) {
	if r.code == 0 {
		r.code = c
	}
	r.ResponseWriter.WriteHeader(c)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) statusCode() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
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
	case "/fnapi/tasks":
		a.apiTasks(w, r)
	case "/fnapi/tasks/act":
		a.apiTasksAct(w, r)
	case "/fnapi/history":
		a.apiHistory(w, r)
	case "/fnapi/history/clear":
		a.apiHistoryClear(w, r)
	case "/fnapi/pending/dismiss":
		a.apiPendingDismiss(w, r)
	case "/fnapi/pending/retry":
		a.apiPendingRetry(w, r)
	case "/fnapi/settings":
		a.apiSettings(w, r)
	case "/fnapi/users":
		a.apiUsers(w, r)
	case "/fnapi/update":
		a.apiUpdate(w, r)
	case "/fnapi/update/status":
		a.apiUpdateStatus(w, r)
	case "/fnapi/update/apply":
		a.apiUpdateApply(w, r)
	case "/fnapi/engine":
		a.apiEngine(w, r)
	case "/fnapi/engine/status":
		a.apiEngineStatus(w, r)
	case "/fnapi/engine/apply":
		a.apiEngineApply(w, r)
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
		"upstream":       currentEngineVersion(a.dataRoot),
		"download_dir":   a.mgr.dirFn(),
		"dir_history":    a.auth.DirHistory(),
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

// 原生面板直接调用下载引擎的接口，不依赖上游页面与内嵌 iframe。
func (a *App) apiTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		code, raw, err := a.mgr.Call(ctx, http.MethodGet, "/api/list", nil)
		if err != nil {
			log.Printf("[tasks] list: %v", err)
			fail(w, http.StatusServiceUnavailable, "下载引擎暂未就绪，请稍后重试")
			return
		}
		if code != http.StatusOK {
			fail(w, http.StatusServiceUnavailable, engineMessage(code))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		a.observeHistory(raw)
		_, _ = w.Write(a.attachPending(raw))
	case http.MethodPost:
		var in struct {
			URL   string `json:"url"`
			Start *bool  `json:"start"`
			Dir   string `json:"dir"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		u := strings.TrimSpace(in.URL)
		if !validDownloadURL(u) {
			fail(w, http.StatusBadRequest, "请粘贴 http(s) 或 magnet 开头的下载地址")
			return
		}
		start := true
		if in.Start != nil {
			start = *in.Start
		}
		dir := strings.TrimSpace(in.Dir)
		if dir != "" {
			d := cleanDir(dir)
			if d == "" || len(d) > 4096 {
				fail(w, http.StatusBadRequest, "下载路径无效，请填写服务器上的绝对路径")
				return
			}
			if err := a.switchDownloadDir(ctx, d); err != nil {
				fail(w, http.StatusBadRequest, "无法使用该下载路径："+err.Error())
				return
			}
			dir = d
		}
		isMagnet := strings.HasPrefix(strings.ToLower(u), "magnet:")
		hash, name := "", ""
		if isMagnet {
			h, err := magnetInfoHash(u)
			if err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
			hash = h
			name = magnetName(u)
			if name == "" {
				name = "磁力任务 " + shortHash(hash)
			}
			// 很多磁力只有 xt 和 dn；补上公开 tracker 再交给引擎。
			u = withPublicTrackers(u)
		}
		code, raw, err := func() (int, []byte, error) {
			// 切换目录会重启引擎，单独给添加请求留出时间。
			callCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			return a.mgr.Call(callCtx, http.MethodPost, "/api/add", map[string]any{"url": u, "start": start})
		}()
		if err != nil {
			log.Printf("[tasks] add: %v", err)
			fail(w, http.StatusServiceUnavailable, "下载引擎暂未就绪，请稍后重试")
			return
		}
		if code != http.StatusOK {
			fail(w, http.StatusBadRequest, engineMessage(code))
			return
		}
		if isMagnet {
			var ans struct {
				Pending bool `json:"pending"`
			}
			if json.Unmarshal(raw, &ans) == nil && ans.Pending {
				a.trackMagnet(u, hash, name, dir)
				writeJSON(w, http.StatusOK, map[string]any{"pending": true, "hash": hash, "name": name})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	default:
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

// observeHistory 把引擎任务列表同步到下载记录。
func (a *App) observeHistory(raw []byte) {
	var list struct {
		Downloads []TaskView `json:"downloads"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	a.hist.Observe(list.Downloads)
}

func (a *App) apiHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": a.hist.Snapshot()})
}

func (a *App) apiHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	if err := a.hist.Clear(); err != nil {
		fail(w, http.StatusInternalServerError, "清空下载记录失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiTasksAct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	var in struct {
		Action string   `json:"action"`
		IDs    []uint64 `json:"ids"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	switch in.Action {
	case "pause", "start", "remove", "delete", "wipe":
	default:
		fail(w, http.StatusBadRequest, "不支持的操作")
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 1000 {
		fail(w, http.StatusBadRequest, "缺少任务编号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	var code int
	var raw []byte
	var err error
	switch in.Action {
	case "remove":
		// “删除下载”：先暂停，再把任务移出列表，磁盘上已有文件保留。
		code, raw, err = a.stopAndAct(ctx, in.IDs, "remove")
	case "wipe", "delete":
		// “无痕删除”：暂停后连文件一起删，再清理残留分片/临时文件与下载记录。
		code, raw, err = a.wipeTasks(ctx, in.IDs)
	default:
		code, raw, err = a.mgr.Call(ctx, http.MethodPost, "/api/act", map[string]any{"action": in.Action, "ids": in.IDs})
	}
	if err != nil {
		log.Printf("[tasks] act %s: %v", in.Action, err)
		fail(w, http.StatusServiceUnavailable, "下载引擎暂未就绪，请稍后重试")
		return
	}
	if code != http.StatusOK {
		fail(w, http.StatusBadRequest, engineMessage(code))
		return
	}
	if in.Action == "remove" {
		a.hist.MarkRemoved(in.IDs)
	}
	if in.Action == "wipe" || in.Action == "delete" {
		a.hist.Forget(in.IDs)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// wipeTasks 先暂停（仍在下载的），等引擎停下后连文件一起删除，
// 再清理引擎可能留下的分片/临时文件，做到“无痕删除”。
func (a *App) wipeTasks(ctx context.Context, ids []uint64) (int, []byte, error) {
	targets := a.collectWipeTargets(ctx, ids)
	code, raw, err := a.stopAndAct(ctx, ids, "delete")
	if err != nil || code != http.StatusOK {
		return code, raw, err
	}
	for _, t := range targets {
		cleanDownloadTraces(t)
	}
	return code, raw, nil
}

// stopAndAct 先暂停任务并等待引擎停下，再执行 remove/delete。
func (a *App) stopAndAct(ctx context.Context, ids []uint64, action string) (int, []byte, error) {
	if _, _, err := a.mgr.Call(ctx, http.MethodPost, "/api/act", map[string]any{"action": "pause", "ids": ids}); err != nil {
		return 0, nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, raw, err := a.mgr.Call(ctx, http.MethodGet, "/api/list", nil)
		if err == nil && code == http.StatusOK && !anyRunning(raw, ids) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return a.mgr.Call(ctx, http.MethodPost, "/api/act", map[string]any{"action": action, "ids": ids})
}

type wipeTarget struct {
	File string
	Path string
}

// collectWipeTargets 在删除前记录文件名与目录，删除后据此清理残留。
func (a *App) collectWipeTargets(ctx context.Context, ids []uint64) []wipeTarget {
	code, raw, err := a.mgr.Call(ctx, http.MethodGet, "/api/list", nil)
	if err != nil || code != http.StatusOK {
		return nil
	}
	var list struct {
		Downloads []struct {
			ID   uint64 `json:"id"`
			File string `json:"file"`
			Path string `json:"path"`
		} `json:"downloads"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []wipeTarget
	for _, d := range list.Downloads {
		if !want[d.ID] || d.File == "" {
			continue
		}
		dir := d.Path
		if dir == "" {
			// 远程接口可能不返回任务路径，退回当前下载目录。
			dir = a.mgr.dirFn()
		}
		if dir != "" {
			out = append(out, wipeTarget{File: d.File, Path: dir})
		}
	}
	return out
}

// cleanDownloadTraces 删除目标文件以及引擎可能留下的同名前缀分片/状态文件。
func cleanDownloadTraces(t wipeTarget) {
	dir := filepath.Clean(t.Path)
	base := filepath.Clean(filepath.Join(dir, t.File))
	if !strings.HasPrefix(base, dir+string(os.PathSeparator)) {
		return
	}
	for _, p := range []string{base, base + ".mdm.part", base + ".mdm.json", base + ".mdm.pieces"} {
		if err := os.RemoveAll(p); err != nil {
			log.Printf("[wipe] 清理 %s 失败：%v", p, err)
		}
	}
}

func anyRunning(raw []byte, ids []uint64) bool {
	var list struct {
		Downloads []struct {
			ID     uint64 `json:"id"`
			Status string `json:"status"`
		} `json:"downloads"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return true
	}
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, d := range list.Downloads {
		if want[d.ID] && (d.Status == "Downloading" || d.Status == "Connecting") {
			return true
		}
	}
	return false
}

// switchDownloadDir 保存新的下载目录；目录变化时重启引擎，并在就绪后恢复之前运行中的任务。
func (a *App) switchDownloadDir(ctx context.Context, dir string) error {
	a.dirMu.Lock()
	defer a.dirMu.Unlock()
	if dir == a.mgr.dirFn() {
		return nil
	}
	if err := prepareDownloadDir(dir); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".mdm-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		return fmt.Errorf("目录不可写：%v", err)
	}
	_ = os.Remove(probe)

	runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var running []uint64
	if code, raw, err := a.mgr.Call(runCtx, http.MethodGet, "/api/list", nil); err == nil && code == http.StatusOK {
		var list struct {
			Downloads []struct {
				ID     uint64 `json:"id"`
				Status string `json:"status"`
			} `json:"downloads"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, d := range list.Downloads {
				if d.Status == "Downloading" || d.Status == "Connecting" {
					running = append(running, d.ID)
				}
			}
		}
	}
	if err := a.auth.SetDownloadDir(dir); err != nil {
		return fmt.Errorf("保存设置失败：%v", err)
	}
	a.mgr.Restart()
	if err := a.mgr.WaitReady(runCtx); err != nil {
		return fmt.Errorf("下载引擎重启超时：%v", err)
	}
	if len(running) > 0 {
		if code, _, err := a.mgr.Call(runCtx, http.MethodPost, "/api/act", map[string]any{"action": "start", "ids": running}); err != nil || code != http.StatusOK {
			log.Printf("[dir] resume %d tasks: code=%d err=%v", len(running), code, err)
		}
	}
	log.Printf("[dir] download dir -> %s (resumed %d)", dir, len(running))
	return nil
}

func validDownloadURL(u string) bool {
	if u == "" || len(u) > 8192 {
		return false
	}
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") ||
		strings.HasPrefix(l, "magnet:?") || strings.HasPrefix(l, "ftp://")
}

func engineMessage(code int) string {
	switch code {
	case http.StatusUnauthorized:
		return "下载引擎尚未完成配对，请点击“重启引擎”后再试"
	case http.StatusForbidden, http.StatusMisdirectedRequest:
		return "下载引擎拒绝了请求，请重启引擎后再试"
	case http.StatusBadRequest:
		return "下载地址无效，请检查后重试"
	case http.StatusConflict:
		return "下载引擎正忙，请稍后再试"
	default:
		return fmt.Sprintf("下载引擎返回错误（HTTP %d），请查看服务日志", code)
	}
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
	if err := a.switchDownloadDir(r.Context(), dir); err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("无法切换下载目录：%v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download_dir": dir, "message": "已保存"})
}

// 共享文件夹通常属于创建它的飞牛用户；应用以 root 运行时新建的目录沿用父目录属主，
// 保证用户在“文件”里也能正常浏览。
func prepareDownloadDir(dir string) error {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s 不是目录", dir)
		}
		return nil
	}
	parent := filepath.Dir(dir)
	uid, gid, hasOwner := dirOwner(parent)
	if err := os.MkdirAll(dir, 0775); err != nil {
		return err
	}
	if hasOwner {
		adoptOwner(dir, uid, gid)
	}
	return nil
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

// apiUpdateStatus 允许未登录访问：升级会重启服务并清空会话，界面需要继续读进度。
func (a *App) apiUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	writeJSON(w, http.StatusOK, a.self.Status())
}

func (a *App) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	info := a.upd.Check(r.Context(), true)
	if info.Error != "" {
		fail(w, http.StatusBadGateway, info.Error)
		return
	}
	if !info.HasUpdate {
		fail(w, http.StatusBadRequest, "当前已是最新版本")
		return
	}
	if info.Asset == "" {
		fail(w, http.StatusBadRequest, "更新包里没有找到 .fpk 安装包，请前往 Gitee 手动下载")
		return
	}
	if err := a.self.Apply(info, info.Latest); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": info.Latest, "message": "已开始升级"})
}

func (a *App) apiEngine(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, http.StatusOK, a.engChk.Check(r.Context(), a.dataRoot, force))
}

// apiEngineStatus 允许未登录访问：内核升级期间界面需要持续读取进度。
func (a *App) apiEngineStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	writeJSON(w, http.StatusOK, a.eng.Status())
}

func (a *App) apiEngineApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	info := a.engChk.Check(r.Context(), a.dataRoot, true)
	if info.Error != "" {
		fail(w, http.StatusBadGateway, info.Error)
		return
	}
	if !info.HasUpdate {
		fail(w, http.StatusBadRequest, "当前内核已是最新版本")
		return
	}
	a.captureRunningForUpdate()
	if err := a.eng.Apply(info); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": info.Latest, "message": "已开始内核升级"})
}

// captureRunningForUpdate 记录正在下载的任务，升级重启后由新进程继续。
func (a *App) captureRunningForUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, raw, err := a.mgr.Call(ctx, http.MethodGet, "/api/list", nil)
	if err != nil || code != http.StatusOK {
		return
	}
	var list struct {
		Downloads []struct {
			ID     uint64 `json:"id"`
			Status string `json:"status"`
		} `json:"downloads"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	var ids []uint64
	for _, d := range list.Downloads {
		if d.Status == "Downloading" || d.Status == "Connecting" {
			ids = append(ids, d.ID)
		}
	}
	path := filepath.Join(a.dataRoot, "update_resume.json")
	if len(ids) == 0 {
		_ = os.Remove(path)
		return
	}
	rawIDs, _ := json.Marshal(map[string]any{"ids": ids})
	_ = os.WriteFile(path, rawIDs, 0600)
	log.Printf("[update] 记录 %d 个运行中的任务，升级后继续", len(ids))
}

// resumeAfterUpdate 在升级重启后恢复之前运行中的任务。
func (a *App) resumeAfterUpdate(ctx context.Context) {
	path := filepath.Join(a.dataRoot, "update_resume.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.Remove(path)
	var in struct {
		IDs []uint64 `json:"ids"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.IDs) == 0 {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	if err := a.mgr.WaitReady(runCtx); err != nil {
		log.Printf("[update] 恢复任务失败：引擎未就绪: %v", err)
		return
	}
	code, _, err := a.mgr.Call(runCtx, http.MethodPost, "/api/act", map[string]any{"action": "start", "ids": in.IDs})
	if err != nil || code != http.StatusOK {
		log.Printf("[update] 恢复 %d 个任务失败: code=%d err=%v", len(in.IDs), code, err)
		return
	}
	log.Printf("[update] 已恢复 %d 个任务", len(in.IDs))
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
