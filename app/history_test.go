package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistoryObserveAndPersist(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	h.Observe([]TaskView{{ID: 7, File: "a.iso", URL: "https://x/a.iso", Path: "/vol2/a.iso", Size: 100, Downloaded: 50, Status: "Downloading"}})
	h.Observe([]TaskView{{ID: 7, File: "a.iso", URL: "https://x/a.iso", Path: "/vol2/a.iso", Size: 100, Downloaded: 100, Status: "Complete"}})
	list := h.Snapshot()
	if len(list) != 1 || list[0].Status != "Complete" || list[0].Downloaded != 100 {
		t.Fatalf("记录未更新: %+v", list)
	}
	// 重新加载后记录应仍在。
	h2 := NewHistory(dir)
	if got := h2.Snapshot(); len(got) != 1 || got[0].Name != "a.iso" {
		t.Fatalf("持久化失败: %+v", got)
	}
}

func TestHistoryForget(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	h.Observe([]TaskView{{ID: 1, File: "one", Status: "Complete"}, {ID: 2, File: "two", Status: "Downloading"}})
	h.Forget([]uint64{2})
	if list := h.Snapshot(); len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("无痕删除应移除记录: %+v", list)
	}
	h2 := NewHistory(dir)
	if list := h2.Snapshot(); len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("删除结果未持久化: %+v", list)
	}
}

func TestHistoryMarkRemovedAndTrim(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory(dir)
	h.Observe([]TaskView{{ID: 1, File: "one", Status: "Complete"}})
	h.MarkRemoved([]uint64{1})
	if list := h.Snapshot(); list[0].Status != "Removed" {
		t.Fatalf("删除状态未记录: %+v", list)
	}
	tasks := make([]TaskView, 0, 520)
	for i := uint64(2); i <= 521; i++ {
		tasks = append(tasks, TaskView{ID: i, File: "f", Status: "Complete"})
	}
	h.Observe(tasks)
	if n := len(h.Snapshot()); n != 500 {
		t.Fatalf("记录应裁剪到 500 条，实际 %d", n)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "history.json"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("history.json 未写入: %v", err)
	}
}
