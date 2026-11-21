package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UpdateStatus 是应用内升级的当前阶段，界面按它绘制进度条。
type UpdateStatus struct {
	State   string `json:"state"` // idle/downloading/verifying/installing/done/error
	Percent int    `json:"percent"`
	Message string `json:"message"`
	Target  string `json:"target,omitempty"`
	Error   string `json:"error,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Total   int64  `json:"total,omitempty"`
	At      int64  `json:"at"`
}

// SelfUpdater 负责下载新版 FPK、校验并交给 fnOS 的 appcenter-cli 安装。
// 安装过程会停掉本应用，因此状态写入磁盘，重启后的新进程继续对外汇报。
type SelfUpdater struct {
	mu     sync.Mutex
	dir    string
	logDir string
	status UpdateStatus
	busy   bool
	http   *http.Client
}

func NewSelfUpdater(dataRoot string) *SelfUpdater {
	return &SelfUpdater{
		dir:    filepath.Join(dataRoot, "updates"),
		logDir: filepath.Join(dataRoot, "logs"),
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

func (s *SelfUpdater) statusPath() string { return filepath.Join(s.dir, "status.json") }

func (s *SelfUpdater) fpkPath(version string) string {
	return filepath.Join(s.dir, fmt.Sprintf("mister-downloader_%s_x86.fpk", strings.TrimPrefix(version, "v")))
}

// Init 读取上一次升级留下的状态：安装完成则标记成功，中断或失败则给出说明。
func (s *SelfUpdater) Init() {
	if raw, err := os.ReadFile(s.statusPath()); err == nil {
		var st UpdateStatus
		if json.Unmarshal(raw, &st) == nil {
			s.status = st
		}
	}
	st := s.status
	cur := strings.TrimPrefix(appVersion, "v")
	target := strings.TrimPrefix(st.Target, "v")
	if st.State == "" {
		st.State = "idle"
	}
	switch st.State {
	case "downloading", "verifying":
		st.State = "error"
		st.Percent = 0
		st.Message = "升级已中断"
		st.Error = "升级过程被中断，请重新点击升级"
	case "installing":
		if sameVersion(cur, target) {
			st.State = "done"
			st.Percent = 100
			st.Message = "已升级到 v" + cur
			st.Error = ""
		} else {
			st.State = "error"
			st.Message = "升级未完成"
			st.Error = "升级未完成，当前仍为 v" + cur + "，请重试或前往 Gitee 手动安装"
		}
	}
	st.At = time.Now().Unix()
	s.status = st
	s.mu.Lock()
	_ = s.saveLocked()
	s.mu.Unlock()
	if st.State == "done" && st.Target != "" {
		_ = os.Remove(s.fpkPath(st.Target))
	}
	_ = filepath.Walk(s.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".part") {
			_ = os.Remove(p)
		}
		return nil
	})
}

func sameVersion(a, b string) bool {
	x, y := versionParts(a), versionParts(b)
	for i := 0; i < 4; i++ {
		var m, n int
		if i < len(x) {
			m = x[i]
		}
		if i < len(y) {
			n = y[i]
		}
		if m != n {
			return false
		}
	}
	return true
}

func (s *SelfUpdater) Status() UpdateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *SelfUpdater) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	s.status.At = time.Now().Unix()
	raw, err := json.MarshalIndent(s.status, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statusPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statusPath())
}

func (s *SelfUpdater) setStatus(state string, percent int, msg string) {
	s.mu.Lock()
	s.status.State = state
	s.status.Percent = percent
	s.status.Message = msg
	s.status.At = time.Now().Unix()
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *SelfUpdater) setError(msg, detail string) {
	s.mu.Lock()
	s.busy = false
	s.status.State = "error"
	s.status.Message = msg
	s.status.Error = detail
	s.status.At = time.Now().Unix()
	_ = s.saveLocked()
	s.mu.Unlock()
	log.Printf("[update] %s: %s", msg, detail)
}

// Apply 启动一次应用内升级，立即返回；实际进度通过 Status 查询。
func (s *SelfUpdater) Apply(info UpdateInfo, target string) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return fmt.Errorf("升级正在进行中，请稍候")
	}
	asset := strings.TrimSpace(info.Asset)
	if asset == "" {
		s.mu.Unlock()
		return fmt.Errorf("更新包里没有找到 .fpk 安装包")
	}
	target = strings.TrimPrefix(strings.TrimSpace(target), "v")
	if target == "" || !sameVersion(target, strings.TrimPrefix(info.Latest, "v")) {
		s.mu.Unlock()
		return fmt.Errorf("更新版本信息不完整")
	}
	s.busy = true
	s.status = UpdateStatus{State: "downloading", Percent: 1, Message: "正在下载更新包…", Target: target}
	_ = s.saveLocked()
	s.mu.Unlock()
	go s.run(info, target)
	return nil
}

func (s *SelfUpdater) run(info UpdateInfo, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		s.setError("无法创建更新目录", err.Error())
		return
	}
	fpk := s.fpkPath(target)
	part := fpk + ".part"
	_ = os.Remove(part)
	_ = os.Remove(fpk)

	sums := s.wantSHA(ctx, info.Latest)
	if err := s.download(ctx, info.Asset, part); err != nil {
		_ = os.Remove(part)
		s.setError("下载更新包失败", err.Error())
		return
	}
	s.setStatus("verifying", 82, "正在校验更新包…")
	if err := verifyFPK(part, target, sums); err != nil {
		_ = os.Remove(part)
		s.setError("更新包校验失败", err.Error())
		return
	}
	if err := os.Rename(part, fpk); err != nil {
		s.setError("保存更新包失败", err.Error())
		return
	}

	cli := findAppcenterCLI()
	if cli == "" {
		s.setError("当前系统不支持应用内升级", "未找到 appcenter-cli，请前往 Gitee 下载安装包后手动安装")
		return
	}
	s.setStatus("installing", 92, "正在安装更新并重启服务…")
	if err := s.install(cli, fpk); err != nil {
		s.setError("无法启动应用内安装", err.Error())
	}
}

func (s *SelfUpdater) wantSHA(ctx context.Context, tag string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://gitee.com/%s/raw/%s/SHA256SUMS.txt", giteeRepo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
	res, err := s.http.Do(req)
	if err != nil {
		log.Printf("[update] 读取校验清单失败（继续用包内校验）：%v", err)
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		log.Printf("[update] 校验清单 HTTP %d（继续用包内校验）", res.StatusCode)
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	ver := strings.TrimPrefix(tag, "v")
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		hash := strings.ToLower(fields[0])
		fname := strings.TrimPrefix(fields[len(fields)-1], "*")
		if len(hash) == 64 && strings.HasSuffix(fname, ".fpk") && strings.Contains(fname, "_"+ver+"_") {
			return hash
		}
	}
	return ""
}

func (s *SelfUpdater) download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
	res, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("连接更新源失败：%v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("更新源返回 HTTP %d", res.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	total := res.ContentLength
	if total <= 0 || total > 300<<20 {
		total = 0
	}
	buf := make([]byte, 256<<10)
	var got int64
	last := time.Now()
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			got += int64(n)
			if got > 300<<20 {
				return fmt.Errorf("更新包超过 300MB，已停止下载")
			}
			if time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				pct := 5
				if total > 0 {
					pct = 5 + int(float64(got)/float64(total)*75)
					if pct > 80 {
						pct = 80
					}
				}
				s.mu.Lock()
				s.status.State = "downloading"
				s.status.Percent = pct
				s.status.Message = "正在下载更新包…"
				s.status.Bytes = got
				s.status.Total = total
				_ = s.saveLocked()
				s.mu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("下载中断：%v", rerr)
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	s.mu.Lock()
	s.status.Bytes = got
	s.status.Total = total
	s.status.Percent = 80
	_ = s.saveLocked()
	s.mu.Unlock()
	return nil
}

func findAppcenterCLI() string {
	if p, err := exec.LookPath("appcenter-cli"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/trim/bin/appcenter-cli", "/usr/local/bin/appcenter-cli", "/usr/bin/appcenter-cli"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// install 用独立会话运行应用中心安装命令：升级会停掉本进程，命令必须继续完成。
func (s *SelfUpdater) install(cli, fpk string) error {
	if err := os.MkdirAll(s.logDir, 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(s.logDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n%s [update] appcenter-cli install-fpk %s\n", time.Now().Format("2006-01-02 15:04:05"), fpk)
	cmd := exec.Command(cli, "install-fpk", fpk)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.status.State != "installing" {
			return
		}
		if err != nil {
			s.busy = false
			s.status.State = "error"
			s.status.Message = "安装更新失败"
			s.status.Error = "应用中心安装命令执行失败，请查看 logs/update.log 或前往 Gitee 手动安装"
			_ = s.saveLocked()
			log.Printf("[update] appcenter-cli exited: %v", err)
		}
	}()
	return nil
}

// verifyFPK 校验外层 manifest、app.tgz 校验和、版本、平台与二进制架构。
func verifyFPK(path, target, wantSHA string) error {
	if wantSHA != "" {
		h, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if !strings.EqualFold(h, wantSHA) {
			return fmt.Errorf("SHA-256 不匹配（下载可能不完整或被篡改）")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("不是有效的 FPK：%v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var manifest []byte
	var appMD5 string
	var appSize int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("更新包已损坏：%v", err)
		}
		name := filepath.ToSlash(strings.TrimPrefix(hdr.Name, "./"))
		switch name {
		case "manifest":
			manifest, _ = io.ReadAll(io.LimitReader(tr, 64<<10))
		case "app.tgz":
			h := md5.New()
			appSize, _ = io.Copy(h, tr)
			appMD5 = hex.EncodeToString(h.Sum(nil))
		}
	}
	if len(manifest) == 0 {
		return fmt.Errorf("缺少 manifest")
	}
	if appMD5 == "" || appSize <= 0 {
		return fmt.Errorf("缺少 app.tgz")
	}
	fields := parseManifest(manifest)
	if fields["appname"] != appID {
		return fmt.Errorf("包名不符：%s", fields["appname"])
	}
	if fields["platform"] != "x86" {
		return fmt.Errorf("不是 x86 安装包")
	}
	if fields["version"] != strings.TrimPrefix(target, "v") {
		return fmt.Errorf("版本不符：%s", fields["version"])
	}
	if !strings.EqualFold(fields["checksum"], appMD5) {
		return fmt.Errorf("app.tgz 校验和不匹配")
	}

	// 从 app.tgz 中确认三个二进制与 ui/config 都存在，并检查主程序架构。
	f.Seek(0, io.SeekStart)
	gz2, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz2.Close()
	tr2 := tar.NewReader(gz2)
	for {
		hdr, err := tr2.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.ToSlash(strings.TrimPrefix(hdr.Name, "./")) != "app.tgz" {
			continue
		}
		igz, err := gzip.NewReader(io.LimitReader(tr2, appSize))
		if err != nil {
			return fmt.Errorf("app.tgz 无法解压：%v", err)
		}
		defer igz.Close()
		itr := tar.NewReader(igz)
		found := map[string]bool{}
		for {
			ih, err := itr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			name := filepath.ToSlash(strings.TrimPrefix(ih.Name, "./"))
			switch name {
			case "mdm-fnos-server":
				head := make([]byte, 64)
				n, _ := io.ReadFull(itr, head)
				if !elfX86Head(head[:n]) {
					return fmt.Errorf("应用主程序不是 x86-64 ELF 文件")
				}
				found[name] = true
			case "misterdownloadmanager", "misterdownloadmanager-cli", "ui/config":
				found[name] = true
			}
		}
		for _, want := range []string{"mdm-fnos-server", "misterdownloadmanager", "misterdownloadmanager-cli", "ui/config"} {
			if !found[want] {
				return fmt.Errorf("更新包缺少 %s", want)
			}
		}
		return nil
	}
	return fmt.Errorf("更新包缺少 app.tgz")
}

func parseManifest(raw []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func elfX86Head(b []byte) bool {
	if len(b) < 20 || string(b[:4]) != "\x7fELF" {
		return false
	}
	return b[4] == 2 && b[18] == 62 && b[19] == 0
}
