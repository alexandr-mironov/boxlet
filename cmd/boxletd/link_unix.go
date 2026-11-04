//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/alexandr-mironov/boxlet/internal/privhelper"
)

// linkDockerSock создаёт /var/run/docker.sock → <socket> —
// стандартный путь, который многие инструменты ожидают по умолчанию.
func linkDockerSock(socket string) error {
	const dst = "/var/run/docker.sock"

	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, _ := os.Readlink(dst); target == socket {
				return nil // уже настроено
			}
		}
		// Что-то другое занимает сокет (например, Docker Desktop) —
		// не трогаем, честно сообщаем.
		return fmt.Errorf("%s уже существует и не указывает на boxlet; "+
			"остановите Docker Desktop/OrbStack или перенастройте вручную", dst)
	}

	// /var/run требует root: сначала пробуем setuid-хелпер boxlet-priv
	// (устанавливается `boxlet priv install`, без пароля в терминале),
	// иначе sudo неинтерактивно.
	if privhelper.Installed() {
		if path, err := privhelper.Path(); err == nil {
			if out, err := exec.Command(path, "link").CombinedOutput(); err != nil {
				return fmt.Errorf("boxlet-priv link: %v (%s)", err, string(out))
			}
			return nil
		}
	}
	cmd := exec.Command("sudo", "-n", "ln", "-s", socket, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("sudo не найден")
		}
		return fmt.Errorf("нужен пароль sudo; установите хелпер: boxlet priv install "+
			"— или выполните вручную: sudo ln -s %s %s (%s)", socket, dst, string(out))
	}
	return nil
}
