// Package privhelper — установка и проверка привилегированного
// setuid-хелпера boxlet-priv (см. cmd/boxlet-priv).
package privhelper

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/alexandr-mironov/boxlet/internal/config"
)

// Path — путь установленного хелпера: ~/.boxlet/bin/boxlet-priv.
// Отдельная копия, а не бинарь из каталога сборки: setuid-бит
// не должен слетать при пересборке проекта.
func Path() (string, error) {
	root, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "bin", "boxlet-priv"), nil
}

// Install копирует бинарь хелпера в Path и выставляет владельца
// root:wheel + setuid-бит. Повышение прав — через osascript с GUI-запросом
// пароля администратора (как у Docker Desktop), sudo в терминале не нужен.
func Install(srcBin string) error {
	if _, err := os.Stat(srcBin); err != nil {
		return fmt.Errorf("бинарь хелпера не найден: %s (соберите: make build)", srcBin)
	}
	dst, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyFile(dst, srcBin); err != nil {
		return err
	}
	// shell-команда в двойных кавычках через strconv.Quote — валидный
	// double-quoted shell-литерал для путей без переводов строк.
	shell := "chown root:wheel " + strconv.Quote(dst) + " && chmod 4755 " + strconv.Quote(dst)
	script := "do shell script " + strconv.Quote(shell) + " with administrator privileges"
	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("установка прав root отклонена или не удалась: %v (%s)", err, out)
	}
	if !Installed() {
		return fmt.Errorf("хелпер установлен в %s, но setuid-бит не применился", dst)
	}
	return nil
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
