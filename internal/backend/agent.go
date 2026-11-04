package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DialFunc открывает соединение с Docker API внутри бэкенда
// (у VM-бэкендов — TLS с клиентским сертификатом boxlet).
type DialFunc func(ctx context.Context) (net.Conn, error)

// DockerClient — HTTP-клиент Docker API поверх dial.
func DockerClient(dial DialFunc, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx)
			},
		},
	}
}

// SyncGuest сбрасывает гостевой page cache на диск: sync() в служебном
// контейнере действует на всё ядро гостевой ОС. Best effort: вызывающий
// при ошибке останавливает VM как есть.
func SyncGuest(dial DialFunc) error {
	if err := RunAgent(dial, []string{"/busybox", "sync"}, false, true); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// PowerOffGuest запускает штатное выключение гостевой ОС: USR2 процессу
// init гостя (busybox init → poweroff). Он выполняет "openrc shutdown",
// local.d/boxlet.stop гасит dockerd и отмонтирует data-диск, затем ядро
// выключает VM. Завершения не ждёт — dockerd гаснет вместе с гостем;
// вызывающий ждёт выхода процесса VM.
//
// Кнопка питания гипервизора тут не годится: в Alpine virt-ядре на arm
// нет драйвера gpio-keys, и гость её не видит.
func PowerOffGuest(dial DialFunc) error {
	if err := RunAgent(dial, []string{"/busybox", "kill", "-USR2", "1"}, true, false); err != nil {
		return fmt.Errorf("poweroff: %w", err)
	}
	return nil
}

// RunAgent запускает команду в служебном контейнере boxlet-agent
// (создаётся bootstrap-скриптом гостевой ОС из статического busybox).
// hostPID — контейнер в pid-namespace гостевой ОС (доступ к её init).
// wait — дождаться завершения и проверить код выхода.
func RunAgent(dial DialFunc, cmd []string, hostPID, wait bool) error {
	client := DockerClient(dial, 20*time.Second)

	hostConfig := map[string]any{"AutoRemove": true}
	if hostPID {
		hostConfig["PidMode"] = "host"
	}
	spec := map[string]any{
		"Image":      "boxlet-agent:latest",
		"Cmd":        cmd,
		"HostConfig": hostConfig,
	}
	payload, _ := json.Marshal(spec)
	resp, err := client.Post("http://boxlet/containers/create", "application/json", bytes.NewReader(payload))
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

	resp, err = client.Post("http://boxlet/containers/"+created.ID+"/start", "application/json", nil)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("start: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if !wait {
		return nil
	}

	// wait отвечает 200 и при ненулевом коде выхода — смотрим StatusCode.
	resp, err = client.Post("http://boxlet/containers/"+created.ID+"/wait", "application/json", nil)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var waited struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if resp.StatusCode >= 400 || json.Unmarshal(data, &waited) != nil {
		return fmt.Errorf("wait: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if waited.Error != nil && waited.Error.Message != "" {
		return errors.New(waited.Error.Message)
	}
	if waited.StatusCode != 0 {
		return fmt.Errorf("код выхода %d", waited.StatusCode)
	}
	return nil
}
