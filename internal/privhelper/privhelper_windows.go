//go:build windows

package privhelper

// Installed на Windows всегда false: setuid-хелпер — механика unix.
func Installed() bool { return false }
