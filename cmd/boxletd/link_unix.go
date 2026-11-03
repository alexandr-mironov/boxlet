//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	// /var/run требует root: используем sudo неинтерактивно; если sudo
	// потребует пароль, вернём понятную инструкцию.
	cmd := exec.Command("sudo", "-n", "ln", "-s", socket, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("sudo не найден")
		}
		return fmt.Errorf("нужен пароль sudo, выполните вручную: sudo ln -s %s %s (%s)",
			socket, dst, string(out))
	}
	return nil
}
