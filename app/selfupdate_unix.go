//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// execSelf 用已替换的新程序覆盖当前进程镜像：PID 与进程归属都不变，
// 不依赖应用中心的启停命令，升级失败时旧进程仍然可用。
func execSelf(appDir string) error {
	exe := filepath.Join(appDir, "mdm-fnos-server")
	return syscall.Exec(exe, []string{exe}, os.Environ())
}
