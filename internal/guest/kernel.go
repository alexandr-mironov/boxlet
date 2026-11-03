package guest

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// EnsureVMLinux готовит несжатое ядро Linux для Virtualization.framework.
//
// Alpine для aarch64 публикует ядро в обёртке EFI zboot (PE32+ со сжатым
// payload внутри), а VZLinuxBootLoader принимает только «сырой» Image.
// Функция находит в zboot-файле gzip-поток с ядром, распаковывает его
// и сохраняет рядом как vmlinux-virt. Повторный вызов ничего не делает,
// если результат уже свежее исходного vmlinuz-virt.
func EnsureVMLinux(assetsDir string) (string, error) {
	src := filepath.Join(assetsDir, "vmlinuz-virt")
	dst := filepath.Join(assetsDir, "vmlinux-virt")

	if si, err := os.Stat(src); err == nil {
		if di, err := os.Stat(dst); err == nil && di.ModTime().After(si.ModTime()) {
			return dst, nil
		}
	} else {
		return "", fmt.Errorf("ядро не найдено: %w", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	kernel, err := extractARM64Image(data)
	if err != nil {
		return "", fmt.Errorf("извлечь ядро из %s: %w", src, err)
	}
	if err := os.WriteFile(dst, kernel, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// gzipMagic — начало gzip-потока (идентификатор + метод deflate).
var gzipMagic = []byte{0x1f, 0x8b, 0x08}

// arm64Magic — сигнатура заголовка ARM64 Image по смещению 0x38.
var arm64Magic = []byte{'A', 'R', 'M', 0x64}

// extractARM64Image ищет в данных zboot-обёртки gzip-поток, после
// распаковки которого получается ARM64 kernel Image.
func extractARM64Image(data []byte) ([]byte, error) {
	for off := 0; ; {
		i := bytes.Index(data[off:], gzipMagic)
		if i < 0 {
			return nil, fmt.Errorf("gzip-поток с ядром не найден")
		}
		off += i
		r, err := gzip.NewReader(bytes.NewReader(data[off:]))
		if err != nil {
			off++
			continue
		}
		// За ядром следуют другие секции PE-файла: не даём читателю
		// искать второй gzip-поток и ругаться на «мусор» в хвосте.
		r.Multistream(false)
		out, err := io.ReadAll(r)
		r.Close()
		if err != nil || len(out) < 0x40 {
			off++
			continue
		}
		if bytes.Equal(out[0x38:0x3c], arm64Magic) {
			return out, nil
		}
		off++
	}
}
