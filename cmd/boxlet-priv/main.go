//go:build darwin

// boxlet-priv — привилегированный помощник boxlet (macOS).
//
// Устанавливается setuid-root (boxlet priv install) и выполняет строго
// две операции, недоступные обычному пользователю:
//
//	boxlet-priv forward   слушать порты <1024 из forwarded_ports конфига
//	                      на 127.0.0.1 и проксировать их в гостевую ОС
//	boxlet-priv link      создать /var/run/docker.sock → сокет boxlet
//
// Меры безопасности:
//   - набор операций фиксирован, произвольные команды не выполняются;
//   - forward: только порты <1024, только из forwarded_ports конфига
//     реального пользователя, не более 8 штук; после bind привилегии
//     сбрасываются (setuid обратно к реальному пользователю);
//   - link: цель симлинка жёстко фиксирована —
//     <домашний каталог реального пользователя>/.boxlet/run/docker.sock.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/proxy"
	"github.com/alexandr-mironov/boxlet/internal/vmnet"
)

const dockerSockDst = "/var/run/docker.sock"

func main() {
	log.SetPrefix("boxlet-priv: ")
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "использование: boxlet-priv <forward|link>\n")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "forward":
		err = cmdForward()
	case "link":
		err = cmdLink()
	default:
		fmt.Fprintf(os.Stderr, "неизвестная операция: %s\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// realUser возвращает запись о реальном пользователе (по real uid,
// а не по переменным окружения — их можно подделать).
func realUser() (*user.User, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return nil, fmt.Errorf("определить реального пользователя: %w", err)
	}
	return u, nil
}

// cmdForward слушает привилегированные порты из forwarded_ports
// и проксирует их в гостевую ОС по её текущему IP (lease vmnet).
func cmdForward() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ports, err := proxy.ParsePortSpecs(cfg.ForwardedPorts)
	if err != nil {
		return err
	}
	priv := map[int]int{}
	for host, guestPort := range ports {
		if host < 1024 {
			priv[host] = guestPort
		}
	}
	if len(priv) == 0 {
		log.Print("нет портов <1024 в forwarded_ports — нечего слушать")
		return nil
	}
	if len(priv) > 8 {
		return fmt.Errorf("слишком много привилегированных портов (%d > 8)", len(priv))
	}

	fwd := proxy.NewPortForwarder(func() (string, error) {
		ip, err := vmnet.LookupLease(cfg.MAC)
		if err != nil || ip == "" {
			return "", fmt.Errorf("IP гостевой ОС неизвестен (lease vmnet): %v", err)
		}
		return ip, nil
	})
	// Bind происходит здесь, пока есть euid=0.
	fwd.SetPorts(priv)

	// Сбрасываем привилегии: дальше процесс работает как обычный
	// пользователь, держа уже открытые слушатели.
	u, err := realUser()
	if err != nil {
		return err
	}
	gid, _ := strconv.Atoi(u.Gid)
	uid, _ := strconv.Atoi(u.Uid)
	if os.Geteuid() == 0 {
		if err := syscall.Setgid(gid); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(uid); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
		log.Printf("привилегии сброшены до %s (uid=%d)", u.Username, uid)
	} else {
		log.Printf("запущен без root: bind портов <1024, скорее всего, не удался (см. выше)")
	}

	log.Printf("проксирую привилегированные порты: %v", keys(priv))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fwd.Close()
	return nil
}

func keys(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cmdLink создаёт /var/run/docker.sock → <домашний каталог
// реального пользователя>/.boxlet/run/docker.sock.
func cmdLink() error {
	u, err := realUser()
	if err != nil {
		return err
	}
	target := u.HomeDir + "/.boxlet/run/docker.sock"

	if fi, err := os.Lstat(dockerSockDst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if cur, _ := os.Readlink(dockerSockDst); cur == target {
				log.Printf("%s уже указывает на %s", dockerSockDst, target)
				return nil
			}
		}
		return fmt.Errorf("%s уже существует и не указывает на boxlet; "+
			"остановите Docker Desktop/OrbStack или перенастройте вручную", dockerSockDst)
	}
	if err := os.Symlink(target, dockerSockDst); err != nil {
		return fmt.Errorf("symlink %s: %w", dockerSockDst, err)
	}
	log.Printf("%s → %s", dockerSockDst, target)
	return nil
}
