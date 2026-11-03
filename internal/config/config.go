// Package config описывает конфигурацию инстанса boxlet (~/.boxlet/config.json).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config — настройки инстанса.
type Config struct {
	// Backend — имя драйвера: darwin-vz, qemu, wsl2, linux.
	// Пусто — дефолт текущей платформы.
	Backend string `json:"backend,omitempty"`

	// CPUs и MemoryMiB — ресурсы VM (игнорируются linux-бэкендом).
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memory_mib"`
	DiskGiB   int `json:"disk_gib"`

	// Mounts — каталоги хоста, доступные VM для bind-mount.
	Mounts []string `json:"mounts,omitempty"`

	// Rosetta — эмуляция linux/amd64 на Apple Silicon.
	Rosetta bool `json:"rosetta"`

	// LinkDockerSock — создавать symlink /var/run/docker.sock.
	LinkDockerSock bool `json:"link_docker_sock"`

	// AutoNetworks — docker-сети, которые создаются автоматически
	// при старте движка.
	AutoNetworks []string `json:"auto_networks,omitempty"`

	// APIPort — порт веб-интерфейса и control API.
	APIPort int `json:"api_port"`

	// AlpineRelease — релиз Alpine для гостевой ОС ("latest-stable" или "3.21").
	AlpineRelease string `json:"alpine_release"`
	// AlpineMirror — зеркало пакетов и netboot-файлов Alpine.
	AlpineMirror string `json:"alpine_mirror"`

	// ForwardedPorts — проброс портов "host:guest" из VM на localhost
	// (для портов, публикуемых контейнерами, напр. 80/443 веб-сервера).
	ForwardedPorts []string `json:"forwarded_ports,omitempty"`

	// DockerPort — порт, на котором dockerd в гостевой ОС слушает TCP,
	// и на который он пробрасывается на 127.0.0.1 хоста.
	DockerPort int `json:"docker_port"`

	// MAC — адрес сетевого интерфейса VM (darwin-vz): по нему демон
	// находит IP гостя в /var/db/dhcpd_leases. Генерируется при первом
	// запуске и сохраняется.
	MAC string `json:"mac,omitempty"`
}

// Default возвращает конфигурацию по умолчанию для текущей платформы.
func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Backend:        DefaultBackend(),
		CPUs:           4,
		MemoryMiB:      4096,
		DiskGiB:        60,
		Mounts:         []string{filepath.Join(home, "projects")},
		Rosetta:        runtime.GOOS == "darwin" && runtime.GOARCH == "arm64",
		LinkDockerSock: true,
		APIPort:        9770,
		AlpineRelease:  "latest-stable",
		AlpineMirror:   "https://dl-cdn.alpinelinux.org/alpine",
		ForwardedPorts: []string{"80:80", "443:443"},
		DockerPort:     2375,
	}
}

// DefaultBackend — драйвер по умолчанию для текущей ОС.
func DefaultBackend() string {
	switch runtime.GOOS {
	case "darwin":
		return "qemu" // darwin-vz появится в M2 (см. docs/roadmap.md)
	case "windows":
		return "wsl2"
	default:
		return "linux"
	}
}

// Dir возвращает корневую директорию инстанса (~/.boxlet или $BOXLET_HOME).
func Dir() (string, error) {
	if d := os.Getenv("BOXLET_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("определить домашнюю директорию: %w", err)
	}
	return filepath.Join(home, ".boxlet"), nil
}

// Path возвращает путь к файлу конфигурации.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load читает конфигурацию; отсутствующий файл — дефолт без ошибки.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("читать %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("разобрать %s: %w", path, err)
	}
	return cfg, nil
}

// Save атомарно сохраняет конфигурацию.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
