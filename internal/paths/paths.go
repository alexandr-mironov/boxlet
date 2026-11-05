// Package paths собирает стандартные пути инстанса boxlet.
package paths

import (
	"os"
	"path/filepath"
)

// ExeDir возвращает каталог текущего исполняемого файла с разрешёнными
// симлинками: boxlet может быть установлен симлинком из /usr/local/bin,
// а остальные бинари (boxletd, boxlet-vm, boxlet-priv, boxlet-tray,
// агенты) лежат рядом с настоящим файлом — искать надо там.
func ExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// Layout — стандартные пути внутри ~/.boxlet.
type Layout struct {
	Root       string
	DockerSock string
	DaemonSock string
	ImagesDir  string
	DataDir    string
	LogsDir    string
	CertsDir   string
}

// New строит раскладку путей от корня инстанса.
func New(root string) Layout {
	return Layout{
		Root:       root,
		DockerSock: filepath.Join(root, "run", "docker.sock"),
		DaemonSock: filepath.Join(root, "run", "boxletd.sock"),
		ImagesDir:  filepath.Join(root, "images"),
		DataDir:    filepath.Join(root, "data"),
		LogsDir:    filepath.Join(root, "logs"),
		CertsDir:   filepath.Join(root, "certs"),
	}
}
