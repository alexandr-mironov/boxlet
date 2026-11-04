package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/dnsd"
	"github.com/alexandr-mironov/boxlet/internal/privhelper"
)

// cmdDNS — управление резолюцией зоны boxlet.local в адрес гостевой ОС.
// DNS-сервер поднимает boxletd автоматически; эта команда лишь включает
// маршрутизацию запросов зоны на него через /etc/resolver/boxlet (macOS).
func cmdDNS(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("использование: boxlet dns install|uninstall|status")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("boxlet dns доступен только на macOS")
	}
	switch args[0] {
	case "install":
		return dnsInstall()
	case "uninstall":
		return dnsUninstall()
	case "status":
		return dnsStatus()
	default:
		return fmt.Errorf("неизвестное действие: %s", args[0])
	}
}

func runPrivResolver(action string) error {
	path, err := privhelper.Path()
	if err != nil {
		return err
	}
	cmd := exec.Command(path, "resolver", action)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// resolverManualCmd — команда для ручной установки резолвера через sudo,
// когда setuid-хелпер не установлен.
func resolverManualCmd() string {
	return fmt.Sprintf("sudo sh -c 'mkdir -p /etc/resolver && printf \"nameserver 127.0.0.1\\nport %d\\n\" > /etc/resolver/boxlet'", dnsd.Port)
}

func dnsInstall() error {
	if !privhelper.Installed() {
		fmt.Println("хелпер boxlet-priv не установлен — два варианта:")
		fmt.Println("  1) boxlet priv install  (рекомендуется: тогда и порты <1024 заработают), затем boxlet dns install")
		fmt.Println("  2) одноразово вручную:  " + resolverManualCmd())
		return fmt.Errorf("требуется boxlet-priv или sudo")
	}
	if err := runPrivResolver("install"); err != nil {
		return err
	}
	fmt.Printf("готово: *.%s теперь резолвится в адрес гостевой ОС\n", dnsd.Zone)
	fmt.Println("проверка: boxlet dns status")
	return nil
}

func dnsUninstall() error {
	if !privhelper.Installed() {
		return fmt.Errorf("требуется boxlet-priv (boxlet priv install) или: sudo rm /etc/resolver/boxlet")
	}
	if err := runPrivResolver("uninstall"); err != nil {
		return err
	}
	fmt.Println("резолвер удалён")
	return nil
}

func dnsStatus() error {
	data, err := os.ReadFile("/etc/resolver/boxlet")
	if err != nil {
		fmt.Println("резолвер не установлен (boxlet dns install)")
		return nil
	}
	fmt.Printf("резолвер установлен:\n%s", data)
	// Реальный запрос к DNS-серверу boxletd.
	ip, err := queryBoxletDNS()
	if err != nil {
		fmt.Printf("DNS-сервер не отвечает (движок запущен? boxlet start): %v\n", err)
		return nil
	}
	fmt.Printf("проверка: test.%s → %s\n", dnsd.Zone, ip)
	return nil
}

// queryBoxletDNS шлёт тестовый A-запрос напрямую серверу boxletd,
// минуя системный резолвер.
func queryBoxletDNS() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return dnsd.Query(ctx, "test."+dnsd.Zone)
}
