// Package paths собирает стандартные пути инстанса boxlet.
package paths

import "path/filepath"

// Layout — стандартные пути внутри ~/.boxlet.
type Layout struct {
	Root        string
	DockerSock  string
	DaemonSock  string
	ImagesDir   string
	DataDir     string
	LogsDir     string
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
	}
}
