//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockForward берёт эксклюзивную блокировку на исполняемый файл хелпера:
// второй `boxlet-priv forward` того же бинаря сразу завершается.
// Файл открывается только на чтение и не создаётся — у setuid-процесса
// нельзя создавать файлы по путям из окружения пользователя.
// Блокировка живёт, пока открыт возвращённый файл (до выхода процесса).
func lockForward(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("открыть %s для блокировки: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("другой boxlet-priv forward уже запущен")
		}
		return nil, fmt.Errorf("блокировка %s: %w", path, err)
	}
	return f, nil
}

// watchParent вызывает onExit, когда родитель (boxletd) завершился:
// на macOS нет PDEATHSIG, а осиротевший процесс получает другого
// родителя (launchd), поэтому сравниваем ppid с исходным.
func watchParent(ppid int, every time.Duration, onExit func()) {
	go func() {
		for {
			time.Sleep(every)
			if os.Getppid() != ppid {
				onExit()
				return
			}
		}
	}()
}
