//go:build windows

package main

import "fmt"

func execSelf(appDir string) error {
	return fmt.Errorf("当前系统不支持应用内自动重启，请手动安装更新包")
}
