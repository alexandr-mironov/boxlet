//go:build windows

package main

import (
	"fmt"
)

// linkDockerSock на Windows не нужен: docker CLI там ходит
// через named pipe, настройка делается в wsl2-драйвере (M3).
func linkDockerSock(socket string) error {
	return fmt.Errorf("на Windows symlink сокета не поддерживается (см. M3, wsl2-драйвер)")
}
