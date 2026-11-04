// Package backend описывает драйвер запуска контейнерного рантайма
// и реестр доступных реализаций.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
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
func AgentExec(ctx context.Context, addr string, argv []string) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("агент гостевой ОС недоступен (%s): %w", addr, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", strings.Join(argv, "\x00")); err != nil {
		return nil, err
	}
	// stdin команды не нужен — закрываем запись, чтобы команда увидела EOF.
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	return io.ReadAll(conn)
}

// SyncGuest сбрасывает гостевой page cache на диск через служебный
// контейнер boxlet-agent (создаётся bootstrap-скриптом гостевой ОС из
// статического busybox). sync() в контейнере действует на всё ядро
// гостевой ОС. addr — адрес dockerd внутри бэкенда (host:port).
// Best effort: при недоступности dockerd возвращает ошибку,
// вызывающий останавливает VM как есть.
func SyncGuest(addr string) error {
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
			},
		},
	}

	body := strings.NewReader(`{"Image":"boxlet-agent:latest","Cmd":["/busybox","sync"],"HostConfig":{"AutoRemove":true}}`)
	resp, err := client.Post("http://boxlet/containers/create", "application/json", body)
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.ID == "" {
		return fmt.Errorf("create: неожиданный ответ: %s", data)
	}

	for _, op := range []string{"start", "wait"} {
		resp, err := client.Post("http://boxlet/containers/"+created.ID+"/"+op, "application/json", nil)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 && op != "wait" {
			return fmt.Errorf("%s: %s", op, resp.Status)
		}
	}
	return nil
}
