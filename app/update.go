package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	giteeOwner        = "wenming760"
	giteeRepo         = giteeOwner + "/mister-downloader-fnos"
	giteeReleasesPage = "https://gitee.com/" + giteeRepo + "/releases"
	giteeLatestAPI    = "https://gitee.com/api/v5/repos/" + giteeRepo + "/releases/latest"

	upstreamOwner        = "anefzaoui"
	upstreamRepoName     = "misterdownloadmanager"
	upstreamReleasesPage = "https://github.com/" + upstreamOwner + "/" + upstreamRepoName + "/releases"
	upstreamLatestAPI    = "https://api.github.com/repos/" + upstreamOwner + "/" + upstreamRepoName + "/releases/latest"

	updateTTL = 10 * time.Minute
)

type UpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"has_update"`
	URL       string `json:"url"`
	Notes     string `json:"notes"`
	Asset     string `json:"asset,omitempty"`
	Source    string `json:"source"`
	CheckedAt int64  `json:"checked_at"`
	Error     string `json:"error,omitempty"`
}

type Updater struct {
	mu   sync.Mutex
	info UpdateInfo
	at   time.Time
	http *http.Client
}

func NewUpdater() *Updater {
	return &Updater{
		http: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		info: UpdateInfo{Current: appVersion, Source: giteeReleasesPage},
	}
}

type giteeRelease struct {
	TagName   string `json:"tag_name"`
	Name      string `json:"name"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Assets    []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *Updater) Check(ctx context.Context, force bool) UpdateInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !force && u.at.After(time.Now().Add(-updateTTL)) && u.info.Error == "" {
		return u.info
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, giteeLatestAPI, nil)
	info := UpdateInfo{
		Current:   appVersion,
		Latest:    "",
		Source:    giteeReleasesPage,
		CheckedAt: time.Now().Unix(),
	}
	if err == nil {
		req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
		req.Header.Set("Accept", "application/json")
		var res *http.Response
		res, err = u.http.Do(req)
		if err == nil {
			defer res.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			if res.StatusCode != http.StatusOK {
				err = fmt.Errorf("Gitee 返回 HTTP %d", res.StatusCode)
			}
			if err == nil {
				var rel giteeRelease
				if jerr := json.Unmarshal(raw, &rel); jerr != nil {
					err = fmt.Errorf("更新源响应无法解析")
				} else if strings.TrimSpace(rel.TagName) == "" {
					err = fmt.Errorf("更新源暂无发布版本")
				} else {
					info.Latest = strings.TrimSpace(rel.TagName)
					info.Notes = trimNotes(rel.Body)
					info.URL = giteeReleasesPage + "/tag/" + info.Latest
					info.HasUpdate = newerVersion(info.Latest, appVersion)
					for _, a := range rel.Assets {
						if strings.HasSuffix(strings.ToLower(a.Name), ".fpk") && a.URL != "" {
							info.Asset = a.URL
							break
						}
					}
				}
			}
		}
	}
	if err != nil {
		info.Error = "检查更新失败：" + err.Error() + "（更新源：" + giteeReleasesPage + "）"
		u.info = info
		u.at = time.Now()
		return info
	}
	u.info = info
	u.at = time.Now()
	return info
}

func trimNotes(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len([]rune(s)) > 1500 {
		r := []rune(s)
		s = string(r[:1500]) + "…"
	}
	return s
}

func versionParts(v string) []int {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	v = strings.TrimPrefix(v, "V")
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	out := make([]int, 0, 4)
	for _, f := range fields {
		n := 0
		for _, c := range f {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		out = []int{0}
	}
	return out
}

func newerVersion(latest, current string) bool {
	a, b := versionParts(latest), versionParts(current)
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// EngineInfo 描述上游下载引擎（内核）的版本信息。
type EngineInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"has_update"`
	URL       string `json:"url"`
	Notes     string `json:"notes"`
	Asset     string `json:"asset,omitempty"`
	ShaAsset  string `json:"-"`
	Source    string `json:"source"`
	CheckedAt int64  `json:"checked_at"`
	Error     string `json:"error,omitempty"`
}

type EngineChecker struct {
	mu   sync.Mutex
	info EngineInfo
	at   time.Time
	http *http.Client
}

func NewEngineChecker() *EngineChecker {
	return &EngineChecker{
		http: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		info: EngineInfo{Current: upstreamVersion, Source: upstreamReleasesPage},
	}
}

func engineVersionPath(dataRoot string) string {
	return filepath.Join(dataRoot, "mdm", "engine-version")
}

// currentEngineVersion 优先读取已安装内核的版本记录，没有记录时使用出厂版本。
func currentEngineVersion(dataRoot string) string {
	if raw, err := os.ReadFile(engineVersionPath(dataRoot)); err == nil {
		v := strings.TrimSpace(string(raw))
		if v != "" {
			return strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
		}
	}
	return upstreamVersion
}

func (c *EngineChecker) Check(ctx context.Context, dataRoot string, force bool) EngineInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := currentEngineVersion(dataRoot)
	if !force && c.at.After(time.Now().Add(-updateTTL)) && c.info.Error == "" && c.info.Current == cur {
		return c.info
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info := EngineInfo{
		Current:   cur,
		Source:    upstreamReleasesPage,
		CheckedAt: time.Now().Unix(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamLatestAPI, nil)
	if err == nil {
		req.Header.Set("User-Agent", "mdm-fnos/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		var res *http.Response
		res, err = c.http.Do(req)
		if err == nil {
			defer res.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			if res.StatusCode != http.StatusOK {
				err = fmt.Errorf("GitHub 返回 HTTP %d", res.StatusCode)
			} else {
				var rel giteeRelease
				if jerr := json.Unmarshal(raw, &rel); jerr != nil {
					err = fmt.Errorf("上游发布信息无法解析")
				} else if strings.TrimSpace(rel.TagName) == "" {
					err = fmt.Errorf("上游暂无发布版本")
				} else {
					info.Latest = strings.TrimSpace(rel.TagName)
					info.Notes = trimNotes(rel.Body)
					info.URL = upstreamReleasesPage + "/tag/" + info.Latest
					info.HasUpdate = newerVersion(info.Latest, cur)
					for _, a := range rel.Assets {
						lower := strings.ToLower(a.Name)
						if strings.Contains(lower, "-linux-x86_64") && strings.HasSuffix(lower, ".tar.gz") && !strings.Contains(lower, "symbols") && a.URL != "" {
							info.Asset = a.URL
						}
						if a.Name == "SHA256SUMS.txt" && a.URL != "" {
							info.ShaAsset = a.URL
						}
					}
				}
			}
		}
	}
	if err != nil {
		info.Error = "检查内核更新失败：" + err.Error() + "（更新源：" + upstreamReleasesPage + "）"
		c.info = info
		c.at = time.Now()
		return info
	}
	c.info = info
	c.at = time.Now()
	return info
}
