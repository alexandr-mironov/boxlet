//go:build darwin

// boxlet-priv — привилегированный помощник boxlet (macOS).
//
// Устанавливается setuid-root (boxlet priv install) и выполняет строго
// две операции, недоступные обычному пользователю:
//
//	boxlet-priv forward <host:guest>...   слушать порты <1024 на 127.0.0.1
//	                                      и проксировать их в гостевую ОС
//	boxlet-priv link                      создать /var/run/docker.sock →
//	                                      сокет boxlet
//	boxlet-priv resolver install|uninstall   (у)становить /etc/resolver/boxlet
//	                                      — зона boxlet.local → DNS boxletd
//
// Меры безопасности:
//   - набор операций фиксирован, произвольные команды не выполняются;
//   - forward: только порты <1024, не более 16; демон передаёт список
//     явно и перезапускает хелпер при его изменении; после bind
//     привилегии сбрасываются (setuid обратно к реальному пользователю);
//   - link: цель симлинка жёстко фиксирована —
//     <домашний каталог реального пользователя>/.boxlet/run/docker.sock;
//   - resolver: путь и содержимое файла жёстко фиксированы,
//     произвольные файлы записать нельзя.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/dnsd"
	"github.com/alexandr-mironov/boxlet/internal/proxy"
	"github.com/alexandr-mironov/boxlet/internal/vmnet"
)

const dockerSockDst = "/var/run/docker.sock"

func main() {
	log.SetPrefix("boxlet-priv: ")
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "использование: boxlet-priv <forward host:guest...|link>\n")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "forward":
		err = cmdForward(os.Args[2:])
	case "link":
		err = cmdLink()
	case "resolver":
		err = cmdResolver(os.Args[2:])
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

// cmdForward слушает привилегированные порты (аргументы host:guest)
// и проксирует их в гостевую ОС по её текущему IP (lease vmnet).
// Список приходит от boxletd: он следит и за конфигом, и за портами,
// опубликованными контейнерами, и перезапускает хелпер при изменениях.
func cmdForward(specs []string) error {
	if len(specs) == 0 {
		log.Print("не передано ни одного порта — нечего слушать")
		return nil
	}
	if len(specs) > 16 {
		return fmt.Errorf("слишком много портов (%d > 16)", len(specs))
	}
	ports, err := proxy.ParsePortSpecs(specs)
	if err != nil {
		return err
	}
	for host := range ports {
		if host >= 1024 {
			return fmt.Errorf("порт %d непривилегированный — для него хелпер не нужен", host)
		}
	}

	// Не больше одного forward одновременно: демон перезапускает хелпер
	// при смене списка портов, и лишняя копия только бы висела, не сумев
	// занять уже занятые порты.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lock, err := lockForward(exe)
	if err != nil {
		return err
	}
	defer lock.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fwd := proxy.NewPortForwarder(func() (string, error) {
		ip, err := vmnet.LookupLease(cfg.MAC)
		if err != nil || ip == "" {
			return "", fmt.Errorf("IP гостевой ОС неизвестен (lease vmnet): %v", err)
		}
		return ip, nil
	})
	// Bind происходит здесь, пока есть euid=0.
	fwd.SetPorts(ports)
	if fwd.Active() == 0 {
		// Держать процесс без единого слушателя незачем: демон увидит
		// выход и повторит попытку с паузой (см. privSupervisor).
		fwd.Close()
		return errors.New("не удалось занять ни одного порта (см. причины выше)")
	}

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

	log.Printf("проксирую привилегированные порты: %v", keys(ports))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	// Демон умер, не остановив хелпер (kill -9, крэш), — выходим сами,
	// иначе хелпер остался бы сиротой, держащим порты.
	parentGone := make(chan struct{})
	watchParent(os.Getppid(), time.Second, func() { close(parentGone) })
	select {
	case <-sig:
	case <-parentGone:
		log.Print("boxletd завершился — выхожу")
	}
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

// resolverFile — конфиг macOS-резолвера зоны boxlet.local: запросы этой
// зоны идут на DNS-сервер boxletd (127.0.0.1, нестандартный порт).
// Путь и содержимое фиксированы — хелпер не умеет писать ничего другого.
const resolverFile = "/etc/resolver/boxlet"

var resolverContent = fmt.Sprintf("nameserver 127.0.0.1\nport %d\n", dnsd.Port)

func cmdResolver(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("использование: boxlet-priv resolver install|uninstall")
	}
	switch args[0] {
	case "install":
		if err := os.MkdirAll("/etc/resolver", 0o755); err != nil {
			return fmt.Errorf("mkdir /etc/resolver: %w", err)
		}
		if err := os.WriteFile(resolverFile, []byte(resolverContent), 0o644); err != nil {
			return fmt.Errorf("запись %s: %w", resolverFile, err)
		}
		log.Printf("%s установлен: зона %s → 127.0.0.1:%d", resolverFile, dnsd.Zone, dnsd.Port)
		return nil
	case "uninstall":
		if err := os.Remove(resolverFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("удаление %s: %w", resolverFile, err)
		}
		log.Printf("%s удалён", resolverFile)
		return nil
	default:
		return fmt.Errorf("неизвестное действие: %s", args[0])
	}
}
