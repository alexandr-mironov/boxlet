// Package backend описывает драйвер запуска контейнерного рантайма
// и реестр доступных реализаций.
package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
)

// Status — состояние бэкенда.
type Status struct {
	Running bool   `json:"running"`
	State   string `json:"state"`   // stopped, starting, running, stopping, error
	Detail  string `json:"detail"`  // пояснение для пользователя
	Version string `json:"version"` // версия dockerd внутри, если известна
}

// Backend — драйвер, поднимающий среду с dockerd
// (VM на macOS/Windows, нативный рантайм на Linux).
type Backend interface {
	// Name — короткое имя драйвера (qemu, darwin-vz, wsl2, linux).
	Name() string

	// Prepare — одноразовая подготовка: скачать образ VM, создать
	// директории, проверить зависимости. Вызывается из `boxlet init`.
	Prepare(ctx context.Context) error

	// Start поднимает среду и дожидается готовности dockerd.
	Start(ctx context.Context) error

	// Stop корректно останавливает среду.
	Stop(ctx context.Context) error

	// Status возвращает текущее состояние.
	Status(ctx context.Context) (Status, error)

	// GuestSocket открывает соединение с Docker API внутри бэкенда.
	// Используется прокси, отдающим сокет наружу на хост.
	GuestSocket(ctx context.Context) (net.Conn, error)

	// Exec выполняет команду внутри бэкенда (диагностика, `boxlet shell`).
	Exec(ctx context.Context, cmd ...string) ([]byte, error)
}

// Porter — бэкенд, чья гостевая ОС доступна с хоста по IP напрямую
// (darwin-vz через vmnet). Позволяет демону пробрасывать TCP-порты
// контейнеров на localhost в userspace, без hostfwd гипервизора.
type Porter interface {
	Backend
	// GuestIP возвращает текущий IP гостевой ОС, маршрутизируемый с хоста.
	GuestIP() (string, error)
}

// Sheller — бэкенд с гостевым агентом (boxlet-agent в VM):
// даёт адрес для `boxlet shell` и интерактивных сессий.
type Sheller interface {
	Backend
	// AgentAddr возвращает host:port гостевого агента, доступный с хоста.
	AgentAddr() (string, error)
}

// Driver — фабрика бэкенда.
type Driver func() Backend

var registry = map[string]Driver{}

// Register вызывается из init() конкретных драйверов.
func Register(name string, d Driver) {
	registry[name] = d
}

// New создаёт бэкенд по имени.
func New(name string) (Backend, error) {
	d, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("неизвестный бэкенд %q (доступны: %v)", name, Available())
	}
	return d(), nil
}

// Available возвращает список зарегистрированных драйверов.
func Available() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}

// AgentExec выполняет команду в гостевой ОС через boxlet-agent
// (одно соединение = одна команда; протокол см. cmd/boxlet-agent).
// Используется реализациями Backend.Exec.
func AgentExec(ctx context.Context, addr, token string, argv []string) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("агент гостевой ОС недоступен (%s): %w", addr, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n%s\n", token, strings.Join(argv, "\x00")); err != nil {
		return nil, err
	}
	// stdin команды не нужен — закрываем запись, чтобы команда увидела EOF.
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	return io.ReadAll(conn)
}
