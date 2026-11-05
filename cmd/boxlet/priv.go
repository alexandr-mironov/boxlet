package main

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/privhelper"
)

// cmdPriv — управление привилегированным setuid-хелпером boxlet-priv.
// Поддержана только установка: она требует пароля администратора один раз
// (GUI-запрос macOS), дальше хелпер работает без паролей.
func cmdPriv(args []string) error {
	if len(args) != 1 || args[0] != "install" {
		return fmt.Errorf("использование: boxlet priv install")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("boxlet priv доступен только на macOS")
	}
	dir, err := paths.ExeDir()
	if err != nil {
		return err
	}
	src := filepath.Join(dir, "boxlet-priv")
	fmt.Println("устанавливаю boxlet-priv (потребуется пароль администратора)…")
	if err := privhelper.Install(src); err != nil {
		return err
	}
	path, _ := privhelper.Path()
	fmt.Printf("готово: %s (root:wheel, setuid)\n", path)
	fmt.Println("теперь доступны: проброс портов <1024 из forwarded_ports и автоматический symlink /var/run/docker.sock")
	return nil
}
