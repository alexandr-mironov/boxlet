package guest

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// AgentToken возвращает токен аутентификации гостевого агента.
// Токен генерируется один раз и хранится в <root>/run/agent-token (0600):
// его знают хост (CLI/демон) и гостевая ОС (токен едет в apkovl).
// Защищает порт агента от других пользователей машины и сторонних
// процессов, не читающих домашний каталог пользователя.
func AgentToken(root string) (string, error) {
	path := filepath.Join(root, "run", "agent-token")
	if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
		return string(data), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return token, nil
}
