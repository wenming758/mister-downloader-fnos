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

// 程序升级只替换这些内容，不触碰 cmd/ 生命周期脚本、config/ 网关配置
// 与下载引擎二进制（引擎由独立的内核升级通道维护）。
var updateItems = []string{"ui", "licenses", "mdm-fnos-server"}

// SelfUpdater 负责程序升级：下载新版 FPK、校验、解包，原子替换文件后
// 直接 exec 新程序。整个过程不依赖应用中心，也不会主动停止自身进程。
type SelfUpdater struct {
	mu     sync.Mutex
	dir    string
	appDir string
	status UpdateStatus
	busy   bool
	http   *http.Client

	// beforeRestart 在替换文件前调用，用于记录运行中的任务并停止下载引擎。
	beforeRestart func()
	// onAbort 在替换或重启失败后调用，用于重新拉起下载引擎。
	onAbort func()
}

func NewSelfUpdater(appDir, dataRoot string) *SelfUpdater {
	return &SelfUpdater{
		dir:    filepath.Join(dataRoot, "updates"),
		appDir: appDir,
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
	// 1.0.8/1.0.9 的安装脚本会留下 install_error.txt，手动安装后仍可能读到旧状态。
	detail := ""
	errFile := filepath.Join(s.dir, "install_error.txt")
	if raw, err := os.ReadFile(errFile); err == nil {
		detail = strings.TrimSpace(string(raw))
		if r := []rune(detail); len(r) > 900 {
			detail = string(r[:900]) + "…"
		}
	}
	_ = os.Remove(errFile)
	_ = os.Remove(filepath.Join(s.dir, "install.sh"))
	// 当前版本已达到或超过目标版本：无论上次停在哪一步，都按升级成功处理。
	if target != "" && (sameVersion(cur, target) || newerVersion(cur, target)) && st.State != "done" {
		st.State = "done"
		st.Percent = 100
		st.Message = "已升级到 v" + cur
		st.Error = ""
	}
	switch st.State {
	case "downloading", "verifying", "installing":
		st.State = "error"
		st.Percent = 0
		st.Message = "升级已中断"
		st.Error = "升级过程被中断，请重新点击升级"
		if detail != "" {
			st.Error = "升级过程被中断（当前仍为 v" + cur + "）：" + detail
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
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".part") {
			_ = os.Remove(p)
		}
		return nil
	})
	if st.State == "done" {
		removeUpgradeDirs(s.dir)
	}
}

// removeUpgradeDirs 清理升级过程生成的解包目录与备份目录。
func removeUpgradeDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), "staged-") || strings.HasPrefix(e.Name(), "backup-") || strings.HasPrefix(e.Name(), "engine-")) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
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

// Apply 启动一次程序升级，立即返回；实际进度通过 Status 查询。
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

	s.setStatus("installing", 88, "正在解包更新包…")
	staged, err := stageFPK(fpk, filepath.Join(s.dir, "staged-"+target))
	if err != nil {
		_ = os.Remove(fpk)
		s.setError("更新包解包失败", err.Error())
		return
	}
	backup := filepath.Join(s.dir, "backup-"+target)
	if err := backupItems(s.appDir, backup); err != nil {
		_ = os.Remove(fpk)
		_ = os.RemoveAll(staged)
		s.setError("备份当前程序失败", err.Error())
		return
	}

	s.setStatus("installing", 93, "正在停止下载引擎并替换程序文件…")
	if s.beforeRestart != nil {
		s.beforeRestart()
	}
	if err := installItems(staged, s.appDir); err != nil {
		s.fail(fpk, staged, backup, "替换程序文件失败", err)
		return
	}
	s.setStatus("installing", 98, "正在重启应用…")
	if err := execSelf(s.appDir); err != nil {
		s.fail(fpk, staged, backup, "无法重启应用", err)
		return
	}
}

// fail 在替换或重启失败时恢复旧文件并重启下载引擎。
func (s *SelfUpdater) fail(fpk, staged, backup, msg string, cause error) {
	if err := restoreItems(backup, s.appDir); err != nil {
		log.Printf("[update] 恢复旧程序失败: %v", err)
	}
	if s.onAbort != nil {
		s.onAbort()
	}
	_ = os.Remove(fpk)
	_ = os.RemoveAll(staged)
	_ = os.RemoveAll(backup)
	s.setError(msg, cause.Error())
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
	return downloadFile(ctx, s.http, url, dst, 300<<20, func(got, total int64) {
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
	})
}

// downloadFile 下载 url 到 dst，超过 max 字节时中止；进度回调按 250ms 节流。
func downloadFile(ctx context.Context, client *http.Client, url, dst string, max int64, progress func(got, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
	res, err := client.Do(req)
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
	if total <= 0 || total > max {
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
			if max > 0 && got > max {
				return fmt.Errorf("更新包超过 %dMB，已停止下载", max>>20)
			}
			if time.Since(last) > 250*time.Millisecond && progress != nil {
				last = time.Now()
				progress(got, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("下载中断：%v", rerr)
		}
	}
	if progress != nil {
		progress(got, total)
	}
	return f.Sync()
}

// stageFPK 把 FPK 里的 app.tgz 解到独立目录，供替换文件使用。
func stageFPK(fpk, dst string) (string, error) {
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return "", err
	}
	f, err := os.Open(fpk)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("不是有效的 FPK：%v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	appTGZ := filepath.Join(dst, "app.tgz")
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("更新包已损坏：%v", err)
		}
		if cleanTarName(hdr.Name) != "app.tgz" || !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		out, err := os.OpenFile(appTGZ, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return "", err
		}
		_, cerr := io.Copy(out, io.LimitReader(tr, 400<<20))
		_ = out.Close()
		if cerr != nil {
			return "", cerr
		}
		found = true
		break
	}
	if !found {
		return "", fmt.Errorf("缺少 app.tgz")
	}
	if err := extractTGZ(appTGZ, dst); err != nil {
		return "", err
	}
	_ = os.Remove(appTGZ)
	for _, name := range updateItems {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			return "", fmt.Errorf("更新包缺少 %s", name)
		}
	}
	return dst, nil
}

func cleanTarName(name string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	return strings.TrimPrefix(name, "/")
}

// extractTGZ 安全解包：拒绝绝对路径与 ..，只写普通文件与目录。
func extractTGZ(path, dst string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("解包失败：%v", err)
		}
		name := cleanTarName(hdr.Name)
		if name == "" {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		rel, err := filepath.Rel(dst, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("更新包包含非法路径：%s", hdr.Name)
		}
		mode := hdr.FileInfo().Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			perm := os.FileMode(0644)
			if mode.Perm()&0111 != 0 {
				perm = 0755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			n, cerr := io.Copy(out, io.LimitReader(tr, 400<<20))
			_ = out.Close()
			if cerr != nil {
				return cerr
			}
			total += n
			if total > 600<<20 {
				return fmt.Errorf("更新包解压后过大")
			}
		}
	}
	return nil
}

func backupItems(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	for _, name := range updateItems {
		from := filepath.Join(src, name)
		if _, err := os.Stat(from); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := copyPath(from, filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func copyPath(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(src, dst, st.Mode().Perm())
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if cerr == nil {
		cerr = out.Sync()
	}
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	return cerr
}

// installItems 用解包目录里的文件原子替换程序目录中的旧文件。
// 先写同目录临时文件再 rename，运行中的可执行文件也能被替换。
func installItems(staged, appDir string) error {
	for _, name := range updateItems {
		src := filepath.Join(staged, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("更新包缺少 %s", name)
		}
		if err := replacePath(src, filepath.Join(appDir, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func restoreItems(backup, appDir string) error {
	entries, err := os.ReadDir(backup)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, e := range entries {
		if err := replacePath(filepath.Join(backup, e.Name()), filepath.Join(appDir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// replacePath 把 src 复制到 dst 所在目录的临时路径，再 rename 覆盖 dst。
func replacePath(src, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".new")
	_ = os.RemoveAll(tmp)
	if err := copyPath(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Windows 不允许 rename 覆盖已有文件，退回先删后改名。
		if err2 := os.RemoveAll(dst); err2 == nil {
			err = os.Rename(tmp, dst)
		}
		if err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
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
