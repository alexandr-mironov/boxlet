//go:build !windows

package main

import "syscall"

// daemonizeAttr отсоединяет демона от сессии терминала.
func daemonizeAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
