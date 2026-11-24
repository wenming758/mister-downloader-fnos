package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EngineUpdater 负责内核（上游下载引擎二进制）升级：只替换
// misterdownloadmanager 与 misterdownloadmanager-cli，不重启应用服务。
type EngineUpdater struct {
	mu       sync.Mutex
	dir      string
	appDir   string
	dataRoot string
	mgr      *Manager
	status   UpdateStatus
	busy     bool
	http     *http.Client
	onReady  func()
}

func NewEngineUpdater(appDir, dataRoot string, mgr *Manager) *EngineUpdater {
	return &EngineUpdater{
		dir:      filepath.Join(dataRoot, "updates"),
		appDir:   appDir,
		dataRoot: dataRoot,
		mgr:      mgr,
		http: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (e *EngineUpdater) statusPath() string { return filepath.Join(e.dir, "engine-status.json") }

// Init 读取上次内核升级状态；进行中被中断则提示重试。
func (e *EngineUpdater) Init() {
	if raw, err := os.ReadFile(e.statusPath()); err == nil {
		var st UpdateStatus
		if json.Unmarshal(raw, &st) == nil {
			e.status = st
		}
	}
	if e.status.State == "" {
		e.status.State = "idle"
	}
	switch e.status.State {
	case "downloading", "verifying", "installing":
		e.status.State = "error"
		e.status.Percent = 0
		e.status.Message = "内核升级已中断"
		e.status.Error = "内核升级过程被中断，请重新点击升级"
	}
	e.status.At = time.Now().Unix()
	e.mu.Lock()
	_ = e.saveLocked()
	e.mu.Unlock()
}

func (e *EngineUpdater) Status() UpdateStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func (e *EngineUpdater) saveLocked() error {
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		return err
	}
	e.status.At = time.Now().Unix()
	raw, err := json.MarshalIndent(e.status, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.statusPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, e.statusPath())
}

func (e *EngineUpdater) setStatus(state string, percent int, msg string) {
	e.mu.Lock()
	e.status.State = state
	e.status.Percent = percent
	e.status.Message = msg
	e.status.Error = ""
	e.status.At = time.Now().Unix()
	_ = e.saveLocked()
	e.mu.Unlock()
}

func (e *EngineUpdater) setError(msg, detail string) {
	e.mu.Lock()
	e.busy = false
	e.status.State = "error"
	e.status.Message = msg
	e.status.Error = detail
	e.status.At = time.Now().Unix()
	_ = e.saveLocked()
	e.mu.Unlock()
	log.Printf("[engine] %s: %s", msg, detail)
}

func (e *EngineUpdater) Apply(info EngineInfo) error {
	e.mu.Lock()
	if e.busy {
		e.mu.Unlock()
		return fmt.Errorf("内核升级正在进行中，请稍候")
	}
	target := strings.TrimPrefix(strings.TrimSpace(info.Latest), "v")
	if target == "" || !sameVersion(target, strings.TrimPrefix(info.Latest, "v")) {
		e.mu.Unlock()
		return fmt.Errorf("内核版本信息不完整")
	}
	if info.Asset == "" {
		e.mu.Unlock()
		return fmt.Errorf("上游发布里没有找到 Linux x86_64 内核包")
	}
	if info.ShaAsset == "" {
		e.mu.Unlock()
		return fmt.Errorf("上游发布里缺少 SHA256SUMS.txt，无法校验内核包")
	}
	if !newerVersion(target, currentEngineVersion(e.dataRoot)) {
		e.mu.Unlock()
		return fmt.Errorf("当前内核已是最新版本")
	}
	e.busy = true
	e.status = UpdateStatus{State: "downloading", Percent: 1, Message: "正在下载内核包…", Target: target}
	_ = e.saveLocked()
	e.mu.Unlock()
	go e.run(info, target)
	return nil
}

func (e *EngineUpdater) run(info EngineInfo, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	work := filepath.Join(e.dir, "engine-"+target)
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0700); err != nil {
		e.setError("无法创建内核升级目录", err.Error())
		return
	}
	pkg := filepath.Join(work, "engine.tar.gz")
	assetName := assetFileName(info.Asset)
	err := downloadFile(ctx, e.http, info.Asset, pkg, 250<<20, func(got, total int64) {
		pct := 3
		if total > 0 {
			pct = 3 + int(float64(got)/float64(total)*67)
			if pct > 70 {
				pct = 70
			}
		}
		e.mu.Lock()
		e.status.State = "downloading"
		e.status.Percent = pct
		e.status.Message = "正在下载内核包…"
		e.status.Bytes = got
		e.status.Total = total
		_ = e.saveLocked()
		e.mu.Unlock()
	})
	if err != nil {
		_ = os.RemoveAll(work)
		e.setError("下载内核包失败", err.Error())
		return
	}
	e.setStatus("verifying", 75, "正在校验内核包…")
	want, err := e.wantSHA(ctx, info.ShaAsset, assetName)
	if err != nil {
		_ = os.RemoveAll(work)
		e.setError("读取内核校验文件失败", err.Error())
		return
	}
	got, err := fileSHA256(pkg)
	if err != nil {
		_ = os.RemoveAll(work)
		e.setError("校验内核包失败", err.Error())
		return
	}
	if !strings.EqualFold(got, want) {
		_ = os.RemoveAll(work)
		e.setError("内核包校验失败", "SHA-256 与上游校验文件不一致，已停止安装")
		return
	}
	staged, err := extractEngine(pkg, filepath.Join(work, "stage"))
	if err != nil {
		_ = os.RemoveAll(work)
		e.setError("解包内核失败", err.Error())
		return
	}
	backup := filepath.Join(work, "backup")
	if err := backupEngine(e.appDir, backup); err != nil {
		_ = os.RemoveAll(work)
		e.setError("备份当前内核失败", err.Error())
		return
	}

	e.setStatus("installing", 85, "正在停止下载引擎…")
	e.mgr.Stop()
	e.setStatus("installing", 90, "正在替换内核文件…")
	oldVersion := currentEngineVersion(e.dataRoot)
	if err := installEngine(staged, e.appDir); err != nil {
		e.rollback(backup, oldVersion)
		_ = os.RemoveAll(work)
		e.setError("替换内核文件失败", err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Join(e.dataRoot, "mdm"), 0700); err != nil {
		e.rollback(backup, oldVersion)
		_ = os.RemoveAll(work)
		e.setError("记录内核版本失败", err.Error())
		return
	}
	if err := os.WriteFile(engineVersionPath(e.dataRoot), []byte(target+"\n"), 0600); err != nil {
		e.rollback(backup, oldVersion)
		_ = os.RemoveAll(work)
		e.setError("记录内核版本失败", err.Error())
		return
	}

	e.setStatus("installing", 95, "正在启动下载引擎…")
	e.mgr.Start()
	readyCtx, readyCancel := context.WithTimeout(ctx, 90*time.Second)
	waitErr := e.mgr.WaitReady(readyCtx)
	readyCancel()
	if waitErr != nil {
		e.rollback(backup, oldVersion)
		_ = os.RemoveAll(work)
		e.setError("新内核启动失败，已恢复旧内核", waitErr.Error())
		return
	}

	e.setStatus("done", 100, "内核已升级到 v"+target)
	e.mu.Lock()
	e.busy = false
	_ = e.saveLocked()
	e.mu.Unlock()
	_ = os.RemoveAll(work)
	if e.onReady != nil {
		go e.onReady()
	}
}

// rollback 恢复旧内核文件并重新启动下载引擎。
func (e *EngineUpdater) rollback(backup, oldVersion string) {
	e.mgr.Stop()
	if err := restoreItems(backup, e.appDir); err != nil {
		log.Printf("[engine] 恢复旧内核失败: %v", err)
	}
	_ = os.WriteFile(engineVersionPath(e.dataRoot), []byte(oldVersion+"\n"), 0600)
	e.mgr.Start()
	if e.onReady != nil {
		go e.onReady()
	}
}

func assetFileName(url string) string {
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return url
	}
	return url[i+1:]
}

// wantSHA 从上游发布的 SHA256SUMS.txt 中取指定文件的校验和。
func (e *EngineUpdater) wantSHA(ctx context.Context, url, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
	res, err := e.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游校验文件返回 HTTP %d", res.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[len(fields)-1], "*") == name && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("校验文件里没有 %s", name)
}

// extractEngine 只从上游 tar.gz 中取出两个 x86-64 引擎二进制。
func extractEngine(path, dst string) (string, error) {
	want := map[string]bool{"misterdownloadmanager": true, "misterdownloadmanager-cli": true}
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("内核包无法解压：%v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("内核包已损坏：%v", err)
		}
		name := filepath.Base(cleanTarName(hdr.Name))
		if !want[name] || !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		out := filepath.Join(dst, name)
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return "", err
		}
		n, cerr := io.Copy(w, io.LimitReader(tr, 200<<20))
		_ = w.Close()
		if cerr != nil {
			return "", cerr
		}
		if n < 4096 {
			return "", fmt.Errorf("内核文件 %s 大小异常", name)
		}
		found[name] = true
	}
	for name := range want {
		if !found[name] {
			return "", fmt.Errorf("内核包缺少 %s", name)
		}
		head := make([]byte, 64)
		hf, err := os.Open(filepath.Join(dst, name))
		if err != nil {
			return "", err
		}
		n, _ := io.ReadFull(hf, head)
		_ = hf.Close()
		if !elfX86Head(head[:n]) {
			return "", fmt.Errorf("内核文件 %s 不是 x86-64 ELF 文件", name)
		}
	}
	return dst, nil
}

func backupEngine(appDir, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	for _, name := range []string{"misterdownloadmanager", "misterdownloadmanager-cli"} {
		if err := copyPath(filepath.Join(appDir, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func installEngine(staged, appDir string) error {
	for _, name := range []string{"misterdownloadmanager", "misterdownloadmanager-cli"} {
		if err := replacePath(filepath.Join(staged, name), filepath.Join(appDir, name)); err != nil {
			return err
		}
	}
	return nil
}
