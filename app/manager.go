package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	mdmPort      = 18643
	mdmLogName   = "mdm.log"
	maxMdmLogLen = 10 << 20
)

type Status struct {
	Running   bool   `json:"running"`
	TokenOK   bool   `json:"token_ok"`
	Downloads int    `json:"downloads"`
	Active    int    `json:"active"`
	Speed     int64  `json:"speed"`
	LastError string `json:"last_error"`
	Port      int    `json:"port"`
	Token     string `json:"mdm_token,omitempty"`
}

type Manager struct {
	appDir   string
	dataRoot string
	mdmData  string
	logsDir  string
	logPath  string
	port     int

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	cmd      *exec.Cmd
	token    string
	lastErr  string
	dirFn    func() string
	stopping bool

	client *http.Client
	target *url.URL
	proxy  *httputil.ReverseProxy
}

func NewManager(appDir, dataRoot string, port int, dirFn func() string) *Manager {
	m := &Manager{
		appDir:   appDir,
		dataRoot: dataRoot,
		mdmData:  filepath.Join(dataRoot, "mdm"),
		logsDir:  filepath.Join(dataRoot, "logs"),
		logPath:  filepath.Join(dataRoot, "logs", mdmLogName),
		port:     port,
		dirFn:    dirFn,
		client: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				Proxy:           nil,
			},
		},
		target: &url.URL{Scheme: "https", Host: fmt.Sprintf("127.0.0.1:%d", port)},
	}
	m.proxy = m.buildProxy()
	return m
}

func (m *Manager) buildProxy() *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(m.target)
	p.Transport = &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConnsPerHost: 16,
		Proxy:               nil,
	}
	base := p.Director
	p.Director = func(r *http.Request) {
		base(r)
		r.Host = m.target.Host
		r.Header.Set("Origin", "https://"+m.target.Host)
		r.Header.Del("Sec-Fetch-Site")
		r.Header.Del("Sec-Fetch-Mode")
		r.Header.Del("Sec-Fetch-Dest")
		r.Header.Set("Authorization", "Bearer "+m.Token())
	}
	p.ModifyResponse = func(res *http.Response) error {
		h := res.Header
		// 让页面可以嵌入飞牛应用外壳。
		h.Del("Content-Security-Policy")
		h.Del("X-Frame-Options")
		h.Del("Cross-Origin-Opener-Policy")
		h.Del("Cross-Origin-Resource-Policy")
		h.Set("Cache-Control", "no-store")
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[panel] %s %s -> upstream error: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "下载引擎暂未就绪：%v\n请稍后刷新，或在“设置”中查看服务日志。", err)
	}
	return p
}

// Call 带令牌访问下载引擎自身的远端接口，供原生面板使用。
func (m *Manager) Call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.target.String()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Host = m.target.Host
	req.Header.Set("Origin", "https://"+m.target.Host)
	req.Header.Set("Authorization", "Bearer "+m.Token())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := m.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, raw, nil
}

// WaitReady 等到引擎能够正常应答任务列表（重启后调用）。
func (m *Manager) WaitReady(ctx context.Context) error {
	var last error
	for {
		code, _, err := m.Call(ctx, http.MethodGet, "/api/list", nil)
		if err == nil && code == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("引擎返回 HTTP %d", code)
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (m *Manager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token
}

func (m *Manager) setToken(t string) {
	m.mu.Lock()
	m.token = t
	m.mu.Unlock()
}

func (m *Manager) setErr(e string) {
	m.mu.Lock()
	m.lastErr = e
	m.mu.Unlock()
}

func (m *Manager) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
}

func (m *Manager) Start() {
	m.mu.Lock()
	if m.done != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.stopping = false
	m.mu.Unlock()
	go func() {
		defer close(done)
		m.supervise(ctx)
	}()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, done, cmd := m.cancel, m.done, m.cmd
	m.cancel, m.done = nil, nil
	m.stopping = true
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		terminate(cmd.Process)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(25 * time.Second):
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}
	m.mu.Lock()
	m.cmd = nil
	m.mu.Unlock()
}

func (m *Manager) Restart() {
	m.Stop()
	m.Start()
}

func terminate(p *os.Process) {
	if runtime.GOOS == "windows" {
		_ = p.Kill()
		return
	}
	// 上游会处理 SIGTERM：保存下载列表后退出；Wait 由 supervise 负责回收。
	_ = p.Signal(syscall.SIGTERM)
}

func (m *Manager) supervise(ctx context.Context) {
	backoff := 3 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cmd, closeLog, err := m.spawn()
		if err != nil {
			m.setErr("下载引擎启动失败：" + err.Error())
			log.Printf("[mdm] spawn: %v", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		m.mu.Lock()
		m.cmd = cmd
		m.mu.Unlock()

		if m.waitReady(ctx, 60*time.Second) {
			if err := m.ensureToken(ctx); err != nil {
				m.setErr("自动配对失败：" + err.Error())
				log.Printf("[mdm] pair: %v", err)
			} else {
				m.setErr("")
				backoff = 3 * time.Second
				log.Printf("[mdm] engine ready, remote api paired")
			}
		} else if ctx.Err() == nil {
			m.setErr("下载引擎启动超时，请查看服务日志。")
		}

		waitErr := cmd.Wait()
		closeLog()
		m.mu.Lock()
		m.cmd = nil
		stopping := m.stopping
		m.mu.Unlock()
		if ctx.Err() != nil || stopping {
			return
		}
		msg := "下载引擎已退出"
		if waitErr != nil {
			msg += "：" + waitErr.Error()
		}
		m.setErr(msg)
		log.Printf("[mdm] exited: %v", waitErr)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (m *Manager) binary(name string) string {
	p := filepath.Join(m.appDir, name)
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(p + ".exe"); err == nil {
			return p + ".exe"
		}
	}
	return p
}

func (m *Manager) spawn() (*exec.Cmd, func(), error) {
	if err := os.MkdirAll(m.mdmData, 0700); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(m.logsDir, 0700); err != nil {
		return nil, nil, err
	}
	dl := strings.TrimSpace(m.dirFn())
	if dl == "" {
		dl = filepath.Join(m.dataRoot, "downloads")
	}
	if err := os.MkdirAll(dl, 0750); err != nil {
		return nil, nil, fmt.Errorf("下载目录不可用：%v", err)
	}
	if st, err := os.Stat(m.logPath); err == nil && st.Size() > maxMdmLogLen {
		_ = os.Rename(m.logPath, m.logPath+".1")
	}
	logFile, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, nil, err
	}
	bin := m.binary("misterdownloadmanager")
	if _, err := os.Stat(bin); err != nil {
		logFile.Close()
		return nil, nil, fmt.Errorf("缺少下载引擎程序 %s", filepath.Base(bin))
	}
	cmd := exec.Command(bin,
		"--headless",
		"--data", m.mdmData,
		"--downloads", dl,
		"--port", fmt.Sprint(m.port),
		"--listen", "127.0.0.1",
		"--hosts", "localhost,127.0.0.1",
	)
	cmd.Dir = m.appDir
	// 不把宿主机的网络代理变量传给下载引擎：下载是否走代理由引擎自身设置决定。
	cmd.Env = append(cleanEnviron(),
		"MDM_DATA_DIR="+m.mdmData,
		"MDM_DOWNLOADS="+dl,
		fmt.Sprintf("MDM_REMOTE_URL=https://127.0.0.1:%d/", m.port),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, nil, err
	}
	log.Printf("[mdm] started pid=%d dir=%s downloads=%s", cmd.Process.Pid, m.mdmData, dl)
	return cmd, func() { _ = logFile.Close() }, nil
}

func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", m.port)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func (m *Manager) tokenPath() string {
	return filepath.Join(m.mdmData, "fnos-token")
}

func (m *Manager) ensureToken(ctx context.Context) error {
	if tok, err := os.ReadFile(m.tokenPath()); err == nil {
		t := strings.TrimSpace(string(tok))
		if t != "" && m.checkToken(ctx, t) {
			m.setToken(t)
			return nil
		}
	}
	code, err := m.requestPairCode(ctx)
	if err != nil {
		return err
	}
	token, err := m.pairWithCode(ctx, code)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.tokenPath(), []byte(token+"\n"), 0600); err != nil {
		log.Printf("[mdm] cannot save token: %v", err)
	}
	m.setToken(token)
	return nil
}

func (m *Manager) checkToken(ctx context.Context, token string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.target.String()+"/api/list", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Host = m.target.Host
	res, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	return res.StatusCode == http.StatusOK
}

var pairCodeRe = regexp.MustCompile(`(\d{4})\s+(\d{4})`)

func (m *Manager) requestPairCode(ctx context.Context) (string, error) {
	var lastOut string
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if !sleepCtx(ctx, 2*time.Second) {
				break
			}
		}
		code, out, err := m.requestPairCodeOnce(ctx)
		if err == nil {
			return code, nil
		}
		lastOut = out
		if ctx.Err() != nil {
			break
		}
	}
	if lastOut == "" {
		return "", fmt.Errorf("获取配对码失败")
	}
	return "", fmt.Errorf("获取配对码失败：%s", strings.TrimSpace(lastOut))
}

func (m *Manager) requestPairCodeOnce(ctx context.Context) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cli := m.binary("misterdownloadmanager-cli")
	cmd := exec.CommandContext(cctx, cli, "pair")
	cmd.Dir = m.appDir
	cmd.Env = append(cleanEnviron(), "MDM_DATA_DIR="+m.mdmData)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", out.String(), err
	}
	text := out.String()
	matches := pairCodeRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return "", text, fmt.Errorf("无法从命令行输出中读取配对码")
	}
	last := matches[len(matches)-1]
	return last[1] + last[2], "", nil
}

func (m *Manager) pairWithCode(ctx context.Context, code string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "name": "fnOS 下载器"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.target.String()+"/api/pair", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://"+m.target.Host)
	req.Host = m.target.Host
	res, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("配对被拒绝（HTTP %d）", res.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("配对响应无效")
	}
	return out.Token, nil
}

func (m *Manager) Watchdog(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !m.isRunning() {
				continue
			}
			tok := m.Token()
			if tok == "" || !m.checkToken(ctx, tok) {
				log.Printf("[mdm] token invalid, re-pairing")
				if err := m.ensureToken(ctx); err != nil {
					log.Printf("[mdm] re-pair: %v", err)
				}
			}
		}
	}
}

func (m *Manager) Summary(ctx context.Context) Status {
	st := Status{Port: m.port}
	m.mu.Lock()
	st.Running = m.cmd != nil
	st.LastError = m.lastErr
	token := m.token
	st.Token = token
	m.mu.Unlock()
	if !st.Running {
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.target.String()+"/api/list", nil)
	if err != nil {
		return st
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Host = m.target.Host
	res, err := m.client.Do(req)
	if err != nil {
		return st
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return st
	}
	st.TokenOK = true
	var data struct {
		Speed     int64 `json:"speed"`
		Downloads []struct {
			Status string `json:"status"`
		} `json:"downloads"`
	}
	if json.Unmarshal(raw, &data) == nil {
		st.Downloads = len(data.Downloads)
		st.Speed = data.Speed
		for _, d := range data.Downloads {
			if d.Status == "Downloading" || d.Status == "Connecting" || d.Status == "Queued" {
				st.Active++
			}
		}
	}
	return st
}

func (m *Manager) TailLog(max int64) string {
	f, err := os.Open(m.logPath)
	if err != nil {
		return "（暂无服务日志）"
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "（日志读取失败）"
	}
	start := st.Size() - max
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "（日志读取失败）"
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

func cleanEnviron() []string {
	drop := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"NO_PROXY": true, "http_proxy": true, "https_proxy": true,
		"all_proxy": true, "no_proxy": true,
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
