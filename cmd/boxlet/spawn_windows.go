//go:build windows

package main

import "syscall"

func daemonizeAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
