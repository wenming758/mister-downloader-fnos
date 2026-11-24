package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// HistoryEntry 是一条下载记录：任务首次出现在引擎列表时创建，
// 之后随轮询更新状态与进度；删除任务后记录保留。
type HistoryEntry struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	AddedAt    int64  `json:"added_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// History 持久化下载记录（最近 500 条），供界面查看与再次下载。
type History struct {
	mu       sync.Mutex
	path     string
	entries  map[uint64]*HistoryEntry
	lastSave time.Time
}

func NewHistory(dataRoot string) *History {
	h := &History{
		path:    filepath.Join(dataRoot, "history.json"),
		entries: make(map[uint64]*HistoryEntry),
	}
	h.load()
	return h
}

func (h *History) load() {
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	var entries []HistoryEntry
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	for i := range entries {
		e := entries[i]
		if e.ID == 0 {
			continue
		}
		h.entries[e.ID] = &e
	}
}

// Snapshot 按最近更新时间倒序返回记录副本。
func (h *History) Snapshot() []HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]HistoryEntry, 0, len(h.entries))
	for _, e := range h.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// TaskView 是引擎任务列表里与记录相关的字段。
type TaskView struct {
	ID         uint64 `json:"id"`
	File       string `json:"file"`
	URL        string `json:"url"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"`
	Error      string `json:"error"`
}

// Observe 用一次引擎列表刷新记录：新任务写入，老任务更新状态与进度。
func (h *History) Observe(tasks []TaskView) {
	if len(tasks) == 0 {
		return
	}
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for _, t := range tasks {
		if t.ID == 0 {
			continue
		}
		e := h.entries[t.ID]
		if e == nil {
			e = &HistoryEntry{ID: t.ID, AddedAt: now, Status: t.Status}
			h.entries[t.ID] = e
			changed = true
		}
		name := strings.TrimSpace(t.File)
		if name == "" {
			name = strings.TrimSpace(t.URL)
		}
		if e.Name != name {
			e.Name = name
			changed = true
		}
		if e.URL != t.URL && t.URL != "" {
			e.URL = t.URL
			changed = true
		}
		if t.Path != "" && e.Path != t.Path {
			e.Path = t.Path
			changed = true
		}
		if t.Size > e.Size {
			e.Size = t.Size
			changed = true
		}
		if t.Downloaded > e.Downloaded {
			e.Downloaded = t.Downloaded
			changed = true
		}
		status := t.Status
		if status == "" {
			status = e.Status
		}
		if e.Status != status {
			e.Status = status
			changed = true
		}
		if e.Error != t.Error {
			e.Error = t.Error
			changed = true
		}
		e.UpdatedAt = now
	}
	h.trimLocked()
	if changed || time.Since(h.lastSave) > time.Minute {
		_ = h.saveLocked()
	}
}

// MarkRemoved 保留记录并把状态改为 Removed。
func (h *History) MarkRemoved(ids []uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if e := h.entries[id]; e != nil {
			e.Status = "Removed"
			e.UpdatedAt = time.Now().Unix()
		}
	}
	_ = h.saveLocked()
}

// Forget 彻底删除记录（无痕删除时使用）。
func (h *History) Forget(ids []uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		delete(h.entries, id)
	}
	_ = h.saveLocked()
}

func (h *History) Clear() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = make(map[uint64]*HistoryEntry)
	return h.saveLocked()
}

func (h *History) trimLocked() {
	if len(h.entries) <= 500 {
		return
	}
	list := make([]*HistoryEntry, 0, len(h.entries))
	for _, e := range h.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt < list[j].UpdatedAt })
	for len(h.entries) > 500 && len(list) > 0 {
		delete(h.entries, list[0].ID)
		list = list[1:]
	}
}

func (h *History) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0700); err != nil {
		return err
	}
	list := make([]HistoryEntry, 0, len(h.entries))
	for _, e := range h.entries {
		list = append(list, *e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt < list[j].UpdatedAt })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.path); err != nil {
		return err
	}
	h.lastSave = time.Now()
	return nil
}
