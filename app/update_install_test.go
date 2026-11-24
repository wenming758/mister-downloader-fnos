package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func fakeELF() []byte {
	b := make([]byte, 8192)
	copy(b, "\x7fELF")
	b[4] = 2   // 64 位
	b[18] = 62 // x86-64
	b[19] = 0
	return b
}

func writeTarGZ(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{Name: name, Mode: 0755, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractEnginePicksBinaries(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "engine.tar.gz")
	writeTarGZ(t, pkg, map[string][]byte{
		"MisterDownloadManager-0.5.2-linux-x86_64/misterdownloadmanager":     fakeELF(),
		"MisterDownloadManager-0.5.2-linux-x86_64/misterdownloadmanager-cli": fakeELF(),
		"MisterDownloadManager-0.5.2-linux-x86_64/README.md":                 []byte("readme"),
	})
	staged, err := extractEngine(pkg, filepath.Join(dir, "stage"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"misterdownloadmanager", "misterdownloadmanager-cli"} {
		if _, err := os.Stat(filepath.Join(staged, name)); err != nil {
			t.Fatalf("缺少 %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(staged, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("不应解出 README.md")
	}
}

func TestExtractEngineRejectsNonELF(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "engine.tar.gz")
	writeTarGZ(t, pkg, map[string][]byte{
		"pkg/misterdownloadmanager":     []byte("not an elf but long enough to pass size check ................................"),
		"pkg/misterdownloadmanager-cli": fakeELF(),
	})
	if _, err := extractEngine(pkg, filepath.Join(dir, "stage")); err == nil {
		t.Fatal("应拒绝非 ELF 文件")
	}
}

func TestStageFPKAndInstall(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldServer := filepath.Join(appDir, "mdm-fnos-server")
	if err := os.WriteFile(oldServer, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "misterdownloadmanager"), []byte("old-engine"), 0755); err != nil {
		t.Fatal(err)
	}

	inner := filepath.Join(dir, "app.tgz")
	writeTarGZ(t, inner, map[string][]byte{
		"mdm-fnos-server":           fakeELF(),
		"misterdownloadmanager":     fakeELF(),
		"misterdownloadmanager-cli": fakeELF(),
		"ui/config":                 []byte("{}"),
		"licenses/LICENSE":          []byte("MIT"),
	})
	fpk := filepath.Join(dir, "test.fpk")
	writeTarGZ(t, fpk, map[string][]byte{"manifest": []byte("appname = mister-downloader\n"), "app.tgz": mustRead(t, inner)})

	staged, err := stageFPK(fpk, filepath.Join(dir, "staged"))
	if err != nil {
		t.Fatal(err)
	}
	if err := installItems(staged, appDir); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(oldServer); string(raw) != string(fakeELF()) {
		t.Fatalf("主程序未替换: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(appDir, "ui", "config")); err != nil {
		t.Fatalf("ui/config 未安装: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
