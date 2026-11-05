package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
)

// cmdSet меняет ресурсы VM в конфиге: boxlet set --cpus 8 --memory-mib 8192.
// Применяется при следующем запуске движка (VM пересоздаётся).
func cmdSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	cpus := fs.Int("cpus", 0, "число CPU виртуальной машины")
	mem := fs.Int("memory-mib", 0, "память VM, MiB")
	disk := fs.Int("disk-gib", 0, "размер диска данных, GiB (только для нового диска)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	changed := []string{}
	if *cpus > 0 {
		cfg.CPUs = *cpus
		changed = append(changed, fmt.Sprintf("cpus=%d", *cpus))
	}
	if *mem > 0 {
		cfg.MemoryMiB = *mem
		changed = append(changed, fmt.Sprintf("memory_mib=%d", *mem))
	}
	if *disk > 0 {
		cfg.DiskGiB = *disk
		changed = append(changed, fmt.Sprintf("disk_gib=%d", *disk))
	}
	if len(changed) == 0 {
		return fmt.Errorf("укажите хотя бы один флаг: --cpus N, --memory-mib N, --disk-gib N\nтекущие: cpus=%d memory_mib=%d disk_gib=%d",
			cfg.CPUs, cfg.MemoryMiB, cfg.DiskGiB)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("конфиг обновлён: %s\n", strings.Join(changed, ", "))
	if *disk > 0 {
		fmt.Println("внимание: размер диска применится только к НОВОМУ диску данных; существующий не меняется")
	}
	fmt.Println("применится после перезапуска: boxlet stop && boxlet start")
	return nil
}

// cmdService — автозапуск boxletd при входе в систему (LaunchAgent):
// boxlet service install | uninstall | status.
func cmdService(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("автозапуск пока поддержан только на macOS (LaunchAgent)")
	}
	if len(args) != 1 {
		return fmt.Errorf("использование: boxlet service <install|uninstall|status>")
	}
	plist, err := launchAgentPath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		return serviceInstall(plist)
	case "uninstall":
		return serviceUninstall(plist)
	case "status":
		if _, err := os.Stat(plist); err == nil {
			fmt.Printf("автозапуск установлен: %s\n", plist)
		} else {
			fmt.Println("автозапуск не установлен (boxlet service install)")
		}
		return nil
	default:
		return fmt.Errorf("неизвестная операция: %s", args[0])
	}
}

const launchAgentLabel = "com.boxlet.boxletd"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

const launchAgentTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

func serviceInstall(plist string) error {
	dir, err := paths.ExeDir()
	if err != nil {
		return err
	}
	boxletd := filepath.Join(dir, "boxletd")
	if _, err := os.Stat(boxletd); err != nil {
		return fmt.Errorf("boxletd не найден рядом с CLI (%s)", boxletd)
	}
	root, err := config.Dir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(root, "logs", "boxletd.log")
	body := fmt.Sprintf(launchAgentTmpl, launchAgentLabel, boxletd, logPath, logPath)

	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	// Пере-загружаем агента (bootout игнорирует отсутствие).
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	execLaunchctl("bootout", domain+"/"+launchAgentLabel)
	if err := execLaunchctl("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Printf("автозапуск установлен: %s\nboxletd стартует при входе в систему и уже запущен\n", plist)
	return nil
}

func serviceUninstall(plist string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if err := execLaunchctl("bootout", domain+"/"+launchAgentLabel); err != nil {
		fmt.Printf("launchctl bootout: %v (продолжаю)\n", err)
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("автозапуск удалён; текущий движок остановлен")
	return nil
}

func execLaunchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cmdUpgrade перекатывает гостевую ОС на свежий релиз Alpine:
// движок останавливается, netboot-файлы скачиваются заново.
// Диск с данными dockerd (образы, контейнеры, тома) не трогается.
func cmdUpgrade(args []string) error {
	// Останавливаем движок, если запущен (ошибка = демон не жив, это нормально).
	_ = apiPost("/api/stop")

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root, err := config.Dir()
	if err != nil {
		return err
	}
	images := filepath.Join(root, "images", "alpine")
	fmt.Printf("удаляю кэш Alpine: %s\n", images)
	if err := os.RemoveAll(images); err != nil {
		return err
	}
	be, err := backend.New(cfg.Backend)
	if err != nil {
		return err
	}
	fmt.Println("скачиваю свежий Alpine (kernel/initramfs/modloop)…")
	if err := be.Prepare(context.Background()); err != nil {
		return err
	}
	fmt.Println("готово. Диск данных сохранён. Запуск: boxlet start")
	return nil
}
