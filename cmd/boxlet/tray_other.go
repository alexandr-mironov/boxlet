//go:build !darwin

package main

import "fmt"

// cmdTray — заглушка для не-macOS платформ: менюбар-трей — фича macOS.
func cmdTray(args []string) error {
	return fmt.Errorf("boxlet tray доступен только на macOS")
}
