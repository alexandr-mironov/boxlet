package guest

import (
	"os"
	"path/filepath"

	"github.com/alexandr-mironov/boxlet/internal/paths"
)

// AgentPort — порт гостевого агента boxlet-agent (см. cmd/boxlet-agent).
// На darwin-vz доступен по IP гостя, на qemu — через hostfwd на 127.0.0.1.
const AgentPort = 2376

// AgentBinary читает статический бинарь гостевого агента для указанной
// архитектуры гостя из каталога рядом с исполняемым файлом
// (bin/boxlet-agent-linux-<goarch>, собирается `make build`).
// Если файла нет — возвращает (nil, nil): VM загрузится без агента,
// `boxlet shell` будет недоступен.
func AgentBinary(goarch string) ([]byte, error) {
	dir, err := paths.ExeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "boxlet-agent-linux-"+goarch)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}
