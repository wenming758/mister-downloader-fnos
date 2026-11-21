//go:build !windows

package main

import "syscall"

// detachAttr 让安装命令脱离当前进程会话，应用被停止后仍能完成升级。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
