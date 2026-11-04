//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// cmdTray запускает иконку boxlet в менюбаре macOS (bin/boxlet-tray).
// Трей опрашивает API демона сам, поэтому демон не обязан быть запущен —
// из меню можно поднять и его.
func cmdTray(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	tray := filepath.Join(filepath.Dir(exe), "boxlet-tray")
	if _, err := os.Stat(tray); err != nil {
		return fmt.Errorf("boxlet-tray не найден рядом с CLI (%s); выполните make build", tray)
	}
	cmd := exec.Command(tray)
	cmd.SysProcAttr = daemonizeAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запустить boxlet-tray: %w", err)
	}
	fmt.Println("иконка boxlet появилась в менюбаре (повторный запуск не создаёт дубликат)")
	return nil
}
