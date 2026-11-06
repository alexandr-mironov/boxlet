//go:build unix

package privhelper

import (
	"os"
	"syscall"
)

// Installed — хелпер существует, принадлежит root и имеет setuid-бит.
func Installed() bool {
	path, err := Path()
	if err != nil {
		return false
	}
	return isSetuidRoot(path)
}

// isSetuidRoot — файл существует, принадлежит root и имеет setuid-бит.
func isSetuidRoot(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == 0 && fi.Mode()&os.ModeSetuid != 0
}
