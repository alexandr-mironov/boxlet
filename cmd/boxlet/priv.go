package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/privhelper"
)

// exitPrivNeedsInstall — код выхода `boxlet priv status`, когда хелпер
// нужно (пере)установить. Отличается от общего кода ошибки 1, чтобы
// install.sh не спутал его со старой версией boxlet без этой команды.
const exitPrivNeedsInstall = 10

// cmdPriv — управление привилегированным setuid-хелпером boxlet-priv.
//
//	boxlet priv install [--if-needed] [--sudo]
//	boxlet priv status
//
// Установка требует пароля администратора (GUI-запрос macOS или, с --sudo,
// sudo в терминале), дальше хелпер работает без паролей.
func cmdPriv(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("boxlet priv доступен только на macOS")
	}
	if len(args) == 0 {
		return errors.New("использование: boxlet priv install [--if-needed] [--sudo] | boxlet priv status")
	}
	dir, err := paths.ExeDir()
	if err != nil {
		return err
	}
	src := filepath.Join(dir, "boxlet-priv")
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	switch args[0] {
	case "status":
		st := privhelper.Status(src, cfg.ForwardedPorts)
		fmt.Printf("privileged helper: %s\n", st)
		if st.NeedsInstall() {
			os.Exit(exitPrivNeedsInstall)
		}
		return nil
	case "install":
		fs := flag.NewFlagSet("priv install", flag.ContinueOnError)
		ifNeeded := fs.Bool("if-needed", false, "ставить, только если хелпер нужен и отличается от текущей сборки")
		sudo := fs.Bool("sudo", false, "спросить пароль через sudo в терминале вместо окна macOS")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *ifNeeded {
			if st := privhelper.Status(src, cfg.ForwardedPorts); !st.NeedsInstall() {
				fmt.Printf("privileged helper: %s — пропускаю\n", st)
				return nil
			}
		}
		fmt.Println("устанавливаю boxlet-priv (потребуется пароль администратора)…")
		if err := privhelper.Install(src, *sudo); err != nil {
			return err
		}
		path, _ := privhelper.Path()
		fmt.Printf("готово: %s (root:wheel, setuid)\n", path)
		fmt.Println("теперь доступны: проброс портов <1024 из forwarded_ports и автоматический symlink /var/run/docker.sock")
		return nil
	default:
		return errors.New("использование: boxlet priv install [--if-needed] [--sudo] | boxlet priv status")
	}
}
