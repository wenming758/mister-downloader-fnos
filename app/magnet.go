package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// 磁力任务先在引擎里等 tracker/DHT；一段时间没有结果，再用公开的种子缓存。
	magnetCacheDelay = 6 * time.Second
	// 缓存里的种子加入后，任务应很快出现在列表里。
	magnetSettle = 35 * time.Second
	// 引擎自身的元数据等待是 180 秒，这里多留一点余量。
	magnetTimeout = 195 * time.Second
	magnetCache   = "https://itorrents.org/torrent/"
	magnetMaxMeta = 8 << 20
)

// 公开 tracker：很多中文站点给出的磁力只有 xt 和 dn，没有 tracker；
// 补上这些能明显提高获取文件列表与 peer 的成功率。
// HTTP(S) 入口优先（国内网络常封 UDP），并保留几个常用的 UDP 入口。
var publicTrackers = []string{
	"https://004430.xyz:443/announce",
	"http://tracker.dler.com:6969/announce",
	"http://bt1.archive.org:6969/announce",
	"http://bt2.archive.org:6969/announce",
	"http://1337.abcvg.info:80/announce",
	"https://1337.abcvg.info:443/announce",
	"https://tracker.nekomi.cn:443/announce",
	"http://tracker.renfei.net:8080/announce",
	"http://tracker.dhitechnical.com:6969/announce",
	"https://tracker.7471.top:443/announce",
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
}

var metaClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

// PendingMagnet 是“已经交给引擎、但文件列表还没拿到”的磁力任务。
// 上游只把这类任务静默地留在内存里，界面看不到；适配层自己记一份并显示出来。
type PendingMagnet struct {
	Hash    string `json:"hash"`
	Name    string `json:"name"`
	Magnet  string `json:"magnet"`
	Dir     string `json:"dir,omitempty"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	Started int64  `json:"started"`
	Elapsed int64  `json:"elapsed"`
}

func shortHash(hash string) string {
	if len(hash) > 10 {
		return hash[:10] + "…"
	}
	return hash
}

// magnetInfoHash 从磁力链接中取出 40 位十六进制的 info hash（btih 支持 hex 与 base32）。
func magnetInfoHash(m string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(m))
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return "", fmt.Errorf("磁力链接格式不正确")
	}
	xt := ""
	for _, v := range u.Query()["xt"] {
		if strings.HasPrefix(strings.ToLower(v), "urn:btih:") {
			xt = strings.TrimSpace(v[len("urn:btih:"):])
			break
		}
	}
	if xt == "" {
		return "", fmt.Errorf("磁力链接里没有 btih 信息")
	}
	switch len(xt) {
	case 40:
		raw, err := hex.DecodeString(xt)
		if err != nil || len(raw) != 20 {
			return "", fmt.Errorf("磁力链接的 btih 不是有效的十六进制")
		}
		return strings.ToLower(xt), nil
	case 32:
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(xt))
		if err != nil || len(raw) != 20 {
			return "", fmt.Errorf("磁力链接的 btih 不是有效的 base32")
		}
		return hex.EncodeToString(raw), nil
	default:
		return "", fmt.Errorf("磁力链接的 btih 长度不正确")
	}
}

// magnetTrackers 取磁力链接里的 tr 列表（已解码，保持原顺序）。
func magnetTrackers(m string) []string {
	u, err := url.Parse(strings.TrimSpace(m))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(publicTrackers)+2)
	for _, t := range u.Query()["tr"] {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func magnetName(m string) string {
	u, err := url.Parse(strings.TrimSpace(m))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("dn"))
}

// withPublicTrackers 补齐公开 tracker；已有的 tracker 不重复添加。
func withPublicTrackers(m string) string {
	u, err := url.Parse(m)
	if err != nil {
		return m
	}
	have := make(map[string]bool)
	for _, t := range u.Query()["tr"] {
		have[strings.TrimSpace(t)] = true
	}
	out := m
	for _, t := range publicTrackers {
		if have[t] {
			continue
		}
		if len(out)+len(t)+8 > 8100 {
			break
		}
		out += "&tr=" + url.QueryEscape(t)
	}
	return out
}

func magnetBase32(hash string) string {
	raw, err := hex.DecodeString(hash)
	if err != nil {
		return ""
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}

// trackMagnet 记录一个等待中的磁力任务并开始盯着它。
func (a *App) trackMagnet(magnet, hash, name, dir string) {
	rec := &PendingMagnet{
		Hash:    hash,
		Name:    name,
		Magnet:  magnet,
		Dir:     dir,
		Stage:   "lookup",
		Message: "正在向 tracker 和对等方查找文件列表…",
		Started: time.Now().Unix(),
	}
	a.pendMu.Lock()
	if old := a.pending[hash]; old != nil && old.Stage != "error" {
		a.pendMu.Unlock()
		return
	}
	a.pending[hash] = rec
	a.pendMu.Unlock()
	log.Printf("[magnet] %s 等待文件列表：%s", shortHash(hash), name)
	go a.watchMagnet(hash, rec)
}

func (a *App) pendingRef(hash string) *PendingMagnet {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	return a.pending[hash]
}

func (a *App) pendingSetStage(hash string, rec *PendingMagnet, stage, msg string) {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	if a.pending[hash] != rec {
		return
	}
	rec.Stage = stage
	rec.Message = msg
}

func (a *App) pendingFail(hash string, rec *PendingMagnet, msg string) {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	if a.pending[hash] != rec {
		return
	}
	rec.Stage = "error"
	rec.Message = msg
	rec.Error = msg
}

func (a *App) pendingDrop(hash string) bool {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	if _, ok := a.pending[hash]; !ok {
		return false
	}
	delete(a.pending, hash)
	return true
}

func (a *App) pendingSnapshot() []PendingMagnet {
	a.pendMu.Lock()
	out := make([]PendingMagnet, 0, len(a.pending))
	now := time.Now().Unix()
	for _, p := range a.pending {
		c := *p
		c.Elapsed = now - p.Started
		if c.Elapsed < 0 {
			c.Elapsed = 0
		}
		out = append(out, c)
	}
	a.pendMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out
}

// attachPending 把等待中的磁力任务并入引擎的任务列表响应。
func (a *App) attachPending(raw []byte) []byte {
	list := a.pendingSnapshot()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	body, err := json.Marshal(list)
	if err != nil {
		return raw
	}
	obj["pending"] = body
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func (a *App) watchMagnet(hash string, rec *PendingMagnet) {
	start := time.Now()
	cacheTried := false
	var settleUntil time.Time
	for {
		if a.pendingRef(hash) != rec {
			return
		}
		if a.taskHasHash(hash) {
			a.pendingDrop(hash)
			log.Printf("[magnet] %s 已加入任务列表", shortHash(hash))
			return
		}
		el := time.Since(start)
		if !cacheTried && el >= magnetCacheDelay {
			cacheTried = true
			a.pendingSetStage(hash, rec, "cache", "正在通过公开种子缓存获取文件列表…")
			trackers := magnetTrackers(rec.Magnet)
			if err := a.addFromCache(context.Background(), hash, trackers); err != nil {
				log.Printf("[magnet] %s 缓存回退失败: %v", shortHash(hash), err)
				a.pendingSetStage(hash, rec, "lookup", "正在等待对等方提供文件列表…")
			} else {
				log.Printf("[magnet] %s 已从公开缓存取得文件列表（%d 个 tracker）", shortHash(hash), len(trackers))
				a.pendingSetStage(hash, rec, "added", "已取得文件列表，正在加入任务…")
				settleUntil = time.Now().Add(magnetSettle)
			}
		}
		if !settleUntil.IsZero() && time.Now().After(settleUntil) {
			a.pendingFail(hash, rec, "已经取到种子文件，但任务没有出现在列表里：下载目录中可能已有同名文件，或引擎拒绝了该任务。")
			log.Printf("[magnet] %s 种子已取得但任务未出现", shortHash(hash))
			return
		}
		if el >= magnetTimeout {
			a.pendingFail(hash, rec, "获取文件列表超时（约 3 分钟）。请确认磁力链接有效，或换成带 tracker 的磁力链接后重试。")
			log.Printf("[magnet] %s 获取文件列表超时", shortHash(hash))
			return
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

func (a *App) taskHasHash(hash string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	code, raw, err := a.mgr.Call(ctx, http.MethodGet, "/api/list", nil)
	if err != nil || code != http.StatusOK {
		return false
	}
	var list struct {
		Downloads []struct {
			URL string `json:"url"`
		} `json:"downloads"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	wantHex := "btih:" + strings.ToLower(hash)
	wantB32 := "btih:" + magnetBase32(hash)
	for _, d := range list.Downloads {
		u := strings.ToLower(d.URL)
		if strings.Contains(u, wantHex) || (wantB32 != "btih:" && strings.Contains(u, wantB32)) {
			return true
		}
	}
	return false
}

// addFromCache 从公开的种子缓存取回 .torrent 并交给引擎；这一步不依赖 DHT 或对等方。
// 缓存里的种子通常没有任何 tracker（来自 DHT 元数据），必须把磁力自带的 tr 与公开 tracker
// 写进 announce/announce-list，否则任务只能靠 DHT 找 peer，在飞牛上会一直 0 速度。
func (a *App) addFromCache(ctx context.Context, hash string, trackers []string) error {
	body, err := a.fetchCachedTorrent(ctx, hash)
	if err != nil {
		return err
	}
	got, err := torrentInfoHash(body)
	if err != nil {
		return fmt.Errorf("缓存返回的不是有效的 torrent：%v", err)
	}
	if !strings.EqualFold(got, hash) {
		return fmt.Errorf("缓存中的种子与磁力哈希不一致")
	}
	with, err := torrentWithTrackers(body, trackers)
	if err != nil {
		return err
	}
	again, err := torrentInfoHash(with)
	if err != nil || !strings.EqualFold(again, hash) {
		return fmt.Errorf("写入 tracker 后种子校验失败")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, raw, err := a.mgr.CallRaw(cctx, http.MethodPost, "/api/torrent?start=true", "application/x-bittorrent", with)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("引擎返回 HTTP %d：%s", code, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (a *App) fetchCachedTorrent(ctx context.Context, hash string) ([]byte, error) {
	target := magnetCache + strings.ToUpper(hash) + ".torrent"
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
	req.Header.Set("Accept", "application/x-bittorrent,*/*")
	res, err := metaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("种子缓存返回 HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, magnetMaxMeta+1))
	if err != nil {
		return nil, err
	}
	if len(body) > magnetMaxMeta {
		return nil, fmt.Errorf("种子文件过大")
	}
	if len(body) == 0 || body[0] != 'd' {
		return nil, fmt.Errorf("种子缓存没有返回 torrent 文件")
	}
	return body, nil
}

func (a *App) apiPendingDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	var in struct {
		Hash string `json:"hash"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	hash := strings.ToLower(strings.TrimSpace(in.Hash))
	if hash == "" {
		fail(w, http.StatusBadRequest, "缺少磁力任务编号")
		return
	}
	if !a.pendingDrop(hash) {
		fail(w, http.StatusNotFound, "该磁力任务已不在等待列表")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) apiPendingRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	if _, ok := a.requireUser(w, r, false); !ok {
		return
	}
	var in struct {
		Hash string `json:"hash"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	hash := strings.ToLower(strings.TrimSpace(in.Hash))
	rec := a.pendingRef(hash)
	if rec == nil {
		fail(w, http.StatusNotFound, "该磁力任务已不在等待列表")
		return
	}
	if rec.Stage != "error" {
		fail(w, http.StatusConflict, "该磁力任务仍在等待中")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	code, raw, err := a.mgr.Call(ctx, http.MethodPost, "/api/add", map[string]any{"url": rec.Magnet, "start": true})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "下载引擎暂未就绪，请稍后重试")
		return
	}
	if code != http.StatusOK {
		fail(w, http.StatusBadRequest, engineMessage(code)+"："+strings.TrimSpace(string(raw)))
		return
	}
	a.pendMu.Lock()
	if a.pending[hash] == rec {
		rec.Stage = "lookup"
		rec.Message = "正在重新查找文件列表…"
		rec.Error = ""
		rec.Started = time.Now().Unix()
	}
	a.pendMu.Unlock()
	go a.watchMagnet(hash, rec)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------------- bencode（只用于校验缓存里的种子） ---------------- */

type bnode struct {
	raw  []byte
	str  string
	dict map[string]*bnode
}

func bparse(b []byte) (*bnode, error) {
	n, _, err := bvalue(b, 0, 0)
	return n, err
}

func bvalue(b []byte, i, depth int) (*bnode, int, error) {
	if depth > 64 {
		return nil, i, fmt.Errorf("bencode 嵌套过深")
	}
	if i >= len(b) {
		return nil, i, fmt.Errorf("bencode 提前结束")
	}
	switch c := b[i]; {
	case c == 'i':
		end := bytes.IndexByte(b[i:], 'e')
		if end < 0 {
			return nil, i, fmt.Errorf("整数没有结束符")
		}
		end += i
		if _, err := strconv.ParseInt(string(b[i+1:end]), 10, 64); err != nil {
			return nil, i, fmt.Errorf("无效的整数")
		}
		return &bnode{raw: b[i : end+1]}, end + 1, nil
	case c >= '0' && c <= '9':
		colon := bytes.IndexByte(b[i:], ':')
		if colon < 0 {
			return nil, i, fmt.Errorf("字符串没有长度分隔符")
		}
		n, err := strconv.Atoi(string(b[i : i+colon]))
		if err != nil || n < 0 {
			return nil, i, fmt.Errorf("无效的字符串长度")
		}
		start := i + colon + 1
		end := start + n
		if end > len(b) {
			return nil, i, fmt.Errorf("字符串超出范围")
		}
		return &bnode{raw: b[i:end], str: string(b[start:end])}, end, nil
	case c == 'l':
		k := i + 1
		for {
			if k >= len(b) {
				return nil, i, fmt.Errorf("列表没有结束符")
			}
			if b[k] == 'e' {
				k++
				break
			}
			_, nk, err := bvalue(b, k, depth+1)
			if err != nil {
				return nil, i, err
			}
			k = nk
		}
		return &bnode{raw: b[i:k]}, k, nil
	case c == 'd':
		node := &bnode{raw: nil, dict: make(map[string]*bnode)}
		k := i + 1
		for {
			if k >= len(b) {
				return nil, i, fmt.Errorf("字典没有结束符")
			}
			if b[k] == 'e' {
				k++
				break
			}
			key, nk, err := bvalue(b, k, depth+1)
			if err != nil {
				return nil, i, err
			}
			if len(key.raw) == 0 || key.raw[0] < '0' || key.raw[0] > '9' {
				return nil, i, fmt.Errorf("字典的键不是字符串")
			}
			val, nk2, err := bvalue(b, nk, depth+1)
			if err != nil {
				return nil, i, err
			}
			node.dict[key.str] = val
			k = nk2
		}
		node.raw = b[i:k]
		return node, k, nil
	}
	return nil, i, fmt.Errorf("无法识别的 bencode 标记 %q", b[i])
}

func torrentInfoHash(body []byte) (string, error) {
	root, err := bparse(body)
	if err != nil {
		return "", err
	}
	if root.dict == nil {
		return "", fmt.Errorf("不是字典")
	}
	info := root.dict["info"]
	if info == nil || len(info.raw) == 0 || info.raw[0] != 'd' {
		return "", fmt.Errorf("缺少 info 段")
	}
	sum := sha1.Sum(info.raw)
	return hex.EncodeToString(sum[:]), nil
}

/* ---------------- 给缓存种子补 tracker ---------------- */

type bentry struct {
	key []byte // 已含长度前缀，可直接拼回 bencode
	raw []byte
}

func bencString(s string) []byte {
	return append([]byte(strconv.Itoa(len(s))+":"), s...)
}

func bencStringValue(raw []byte) (string, bool) {
	colon := bytes.IndexByte(raw, ':')
	if colon <= 0 {
		return "", false
	}
	n, err := strconv.Atoi(string(raw[:colon]))
	if err != nil || n < 0 || colon+1+n != len(raw) {
		return "", false
	}
	return string(raw[colon+1:]), true
}

// btopLevel 拆出顶层字典的每个键值对，值是原始 bencode 字节（info 段因此保持不变）。
func btopLevel(b []byte) ([]bentry, error) {
	if len(b) == 0 || b[0] != 'd' {
		return nil, fmt.Errorf("不是种子文件")
	}
	var out []bentry
	i := 1
	for {
		if i >= len(b) {
			return nil, fmt.Errorf("种子文件不完整")
		}
		if b[i] == 'e' {
			i++
			break
		}
		k, nk, err := bvalue(b, i, 0)
		if err != nil {
			return nil, err
		}
		if len(k.raw) == 0 || k.raw[0] < '0' || k.raw[0] > '9' {
			return nil, fmt.Errorf("种子的键不是字符串")
		}
		v, nv, err := bvalue(b, nk, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, bentry{key: k.raw, raw: v.raw})
		i = nv
	}
	if i != len(b) {
		return nil, fmt.Errorf("种子文件末尾有多余内容")
	}
	return out, nil
}

func bListStrings(raw []byte) []string {
	if len(raw) == 0 || raw[0] != 'l' {
		return nil
	}
	var out []string
	for i := 1; i < len(raw) && raw[i] != 'e'; {
		v, nv, err := bvalue(raw, i, 0)
		if err != nil {
			return out
		}
		if len(v.raw) > 0 && v.raw[0] == 'l' {
			out = append(out, bListStrings(v.raw)...)
		} else if s, ok := bencStringValue(v.raw); ok {
			out = append(out, s)
		}
		i = nv
	}
	return out
}

func torrentTrackers(entries []bentry) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, e := range entries {
		k, ok := bencStringValue(e.key)
		if !ok {
			continue
		}
		switch k {
		case "announce":
			if s, ok := bencStringValue(e.raw); ok {
				add(s)
			}
		case "announce-list":
			for _, s := range bListStrings(e.raw) {
				add(s)
			}
		}
	}
	return out
}

// torrentWithTrackers 把 extra 里缺少的 tracker 写进 announce/announce-list。
// info 段保持原始字节，info hash 不变（返回前由调用方再校验一次）。
func torrentWithTrackers(body []byte, extra []string) ([]byte, error) {
	entries, err := btopLevel(body)
	if err != nil {
		return nil, err
	}
	all := torrentTrackers(entries)
	seen := make(map[string]bool, len(all))
	for _, t := range all {
		seen[t] = true
	}
	changed := false
	for _, t := range extra {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] || len(t) > 2048 {
			continue
		}
		seen[t] = true
		all = append(all, t)
		changed = true
	}
	if len(all) == 0 || !changed {
		return body, nil
	}
	out := make([]bentry, 0, len(entries)+1)
	for _, e := range entries {
		if k, ok := bencStringValue(e.key); ok && (k == "announce" || k == "announce-list") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, bentry{key: bencString("announce"), raw: bencString(all[0])})
	list := []byte{'l'}
	for _, t := range all {
		list = append(list, 'l')
		list = append(list, bencString(t)...)
		list = append(list, 'e')
	}
	list = append(list, 'e')
	out = append(out, bentry{key: bencString("announce-list"), raw: list})
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].key, out[j].key) < 0 })
	buf := []byte{'d'}
	for _, e := range out {
		buf = append(buf, e.key...)
		buf = append(buf, e.raw...)
	}
	buf = append(buf, 'e')
	return buf, nil
}
