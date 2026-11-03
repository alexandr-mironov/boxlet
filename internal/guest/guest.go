// Package guest отвечает за гостевую ОС boxlet: скачивание netboot-файлов
// Alpine Linux (ядро, initramfs, modloop), генерацию apkovl-оверлея
// с автонастройкой dockerd и HTTP-сервер netboot для первой загрузки VM.
package guest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// netbootFiles — файлы Alpine, необходимые для загрузки VM.
var netbootFiles = []string{"vmlinuz-virt", "initramfs-virt", "modloop-virt"}

// Assets — пути к скачанным файлам гостевой ОС.
type Assets struct {
	Dir     string
	Kernel  string
	Initrd  string
	Modloop string // подключается в VM отдельным диском как источник модулей ядра
}

// AlpineArch маппит GOARCH на архитектуру Alpine.
func AlpineArch(goarch string) (string, error) {
	switch goarch {
	case "arm64":
		return "aarch64", nil
	case "amd64":
		return "x86_64", nil
	default:
		return "", fmt.Errorf("архитектура %s не поддерживается гостевой ОС", goarch)
	}
}

// Ensure скачивает недостающие netboot-файлы Alpine в imagesDir
// и возвращает пути к ним. Прогресс пишется в logw.
func Ensure(ctx context.Context, imagesDir, release, mirror string, logw io.Writer) (*Assets, error) {
	arch, err := AlpineArch(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(imagesDir, "alpine", arch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	a := &Assets{
		Dir:     dir,
		Kernel:  filepath.Join(dir, "vmlinuz-virt"),
		Initrd:  filepath.Join(dir, "initramfs-virt"),
		Modloop: filepath.Join(dir, "modloop-virt"),
	}

	base := fmt.Sprintf("%s/%s/releases/%s/netboot", mirror, release, arch)
	for _, name := range netbootFiles {
		dst := filepath.Join(dir, name)
		if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
			continue // уже скачан
		}
		url := base + "/" + name
		fmt.Fprintf(logw, "скачиваю %s\n", url)
		if err := download(ctx, url, dst); err != nil {
			return nil, fmt.Errorf("скачать %s: %w", name, err)
		}
	}
	return a, nil
}

// download скачивает файл по HTTP во временный файл и атомарно переименовывает.
func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
