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
	errFile := filepath.Join(s.dir, "install_error.txt")
	detail := ""
	if raw, err := os.ReadFile(errFile); err == nil {
		detail = strings.TrimSpace(string(raw))
		if r := []rune(detail); len(r) > 900 {
			detail = string(r[:900]) + "…"
		}
	}
	_ = os.Remove(errFile)
	// 版本已经是目标版本：无论上次停在哪一步，都按升级成功处理。
	if target != "" && sameVersion(cur, target) && st.State != "done" {
		st.State = "done"
		st.Percent = 100
		st.Message = "已升级到 v" + cur
		st.Error = ""
	}
	switch st.State {
	case "downloading", "verifying":
		st.State = "error"
		st.Percent = 0
		st.Message = "升级已中断"
		st.Error = "升级过程被中断，请重新点击升级"
	case "installing":
		st.State = "error"
		st.Message = "升级未完成"
		st.Error = "升级未完成，当前仍为 v" + cur + "，请重试或前往 Gitee 手动安装"
		if detail != "" {
			st.Error = "升级未完成（当前仍为 v" + cur + "）：" + detail
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
	_ = os.Remove(filepath.Join(s.dir, "install_error.txt"))
	s.setStatus("installing", 92, "正在安装更新并重启服务…")
	// 安装必须由独立脚本完成：它会先停掉本进程，避免应用中心等待运行中的应用。
	if err := s.startHelper(cli, fpk, target); err != nil {
		s.setError("无法启动应用内安装", err.Error())
		return
	}
	s.watchInstall()
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

// installHelper 是升级安装脚本。由服务器以独立会话启动，先停止应用再调用应用中心；
// 安装命令限时 240 秒，失败或未生效时直接把 FPK 里的 app.tgz 解包覆盖，最后重新启动应用。
const installHelper = `#!/bin/sh
set -u
CLI="$1"; FPK="$2"; TARGET="$3"; APPDIR="$4"; DATADIR="$5"; MAIN="$6"
LOG="$DATADIR/logs/update.log"
ERRF="$DATADIR/updates/install_error.txt"
rm -f "$ERRF"
log() { echo "$(date '+%Y-%m-%d %H:%M:%S') [install] $*" >> "$LOG" 2>/dev/null; }
note() { printf '%s\n' "$*" > "$ERRF" 2>/dev/null; }

log "开始升级到 v$TARGET：$FPK"
TMP="$DATADIR/updates/unpack.$$"
rm -rf "$TMP"; mkdir -p "$TMP" 2>/dev/null || true
PKG="$FPK"
# 复制一份再解包：新版服务启动后会删掉 updates/ 下的安装包。
if cp "$FPK" "$TMP/pkg.fpk" 2>/dev/null; then PKG="$TMP/pkg.fpk"; fi
"$MAIN" stop >>"$LOG" 2>&1 || true

rc=127
if [ -x "$CLI" ]; then
  if command -v timeout >/dev/null 2>&1; then
    timeout 240 "$CLI" install-fpk "$FPK" </dev/null >>"$LOG" 2>&1
    rc=$?
  else
    "$CLI" install-fpk "$FPK" </dev/null >>"$LOG" 2>&1 &
    pid=$!
    n=0
    while kill -0 "$pid" 2>/dev/null && [ "$n" -lt 240 ]; do sleep 1; n=$((n+1)); done
    if kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null; sleep 2; kill -KILL "$pid" 2>/dev/null
      rc=124
    else
      wait "$pid"; rc=$?
    fi
  fi
fi
log "install-fpk 退出码 $rc"

ok=0
if tar -xzf "$PKG" -C "$TMP" app.tgz >>"$LOG" 2>&1; then
  mkdir -p "$TMP/app" 2>/dev/null || true
  if tar -xzf "$TMP/app.tgz" -C "$TMP/app" >>"$LOG" 2>&1; then
    if [ -f "$TMP/app/mdm-fnos-server" ] && cmp -s "$TMP/app/mdm-fnos-server" "$APPDIR/mdm-fnos-server"; then
      ok=1
      log "应用中心安装已生效"
    fi
  else
    log "app.tgz 解包失败"
  fi
else
  log "FPK 解包失败"
fi

if [ "$ok" != "1" ] && [ -f "$TMP/app/mdm-fnos-server" ]; then
  log "改为本地解包覆盖 $APPDIR"
  for item in mdm-fnos-server misterdownloadmanager misterdownloadmanager-cli ui licenses; do
    [ -e "$TMP/app/$item" ] || continue
    cp -a "$TMP/app/$item" "$APPDIR/" >>"$LOG" 2>&1 || ok=0
  done
  [ -f "$APPDIR/mdm-fnos-server" ] && cmp -s "$TMP/app/mdm-fnos-server" "$APPDIR/mdm-fnos-server" && ok=1
fi
rm -rf "$TMP"

if [ "$ok" = "1" ]; then
  chmod 0755 "$APPDIR/mdm-fnos-server" "$APPDIR/misterdownloadmanager" "$APPDIR/misterdownloadmanager-cli" 2>/dev/null || true
elif [ "$rc" = "124" ]; then
  note "应用中心安装命令超过 240 秒未返回，请通过应用中心手动安装（更新包已保存在应用数据目录 updates/ 下）"
else
  note "应用中心安装失败（退出码 $rc），请通过应用中心手动安装（更新包已保存在应用数据目录 updates/ 下）"
fi

log "启动应用（ok=$ok）"
"$MAIN" start >>"$LOG" 2>&1 || true
if [ "$ok" != "1" ]; then
  tail -n 12 "$LOG" 2>/dev/null >>"$ERRF"
fi
log "升级脚本结束（ok=$ok）"
exit 0
`

func (s *SelfUpdater) startHelper(cli, fpk, target string) error {
	appDir := os.Getenv("APP_DIR")
	if appDir == "" {
		if exe, err := os.Executable(); err == nil {
			appDir = filepath.Dir(exe)
		}
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Dir(s.dir)
	}
	if appDir == "" || dataDir == "" {
		return fmt.Errorf("无法确定应用目录，请手动安装更新包")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	script := filepath.Join(s.dir, "install.sh")
	if err := os.WriteFile(script, []byte(installHelper), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.logDir, 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(s.logDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return err
	}
	fmt.Fprintf(logf, "\n%s [update] 启动升级脚本：v%s -> %s\n",
		time.Now().Format("2006-01-02 15:04:05"), appVersion, fpk)
	cmd := exec.Command("/bin/sh", script, cli, fpk, target, appDir, dataDir, filepath.Join(appDir, "cmd", "main"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachAttr()
	err = cmd.Start()
	_ = logf.Close()
	return err
}

// watchInstall 防止安装命令静默挂起：超过 6 分钟仍未离开 installing 就报错并给出日志路径。
func (s *SelfUpdater) watchInstall() {
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		if s.Status().State != "installing" {
			return
		}
	}
	if s.Status().State == "installing" {
		s.setError("安装没有完成", "安装命令长时间未返回。请重启应用后重试，或通过应用中心手动安装更新包（已保存在应用数据目录 updates/ 下）")
	}
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
