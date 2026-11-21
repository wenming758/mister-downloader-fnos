//go:build !windows

package main

import (
	"os"
	"syscall"
)

func dirOwner(path string) (int, int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(sys.Uid), int(sys.Gid), true
}

func adoptOwner(path string, uid, gid int) {
	_ = os.Chown(path, uid, gid)
}
