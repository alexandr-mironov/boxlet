// Package linux реализует нативный Linux-бэкенд без VM:
// управление системным (или rootless) dockerd через systemd.
package linux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
)

const socketPath = "/var/run/docker.sock"

type Backend struct{}

func New() backend.Backend { return &Backend{} }

func (b *Backend) Name() string { return "linux" }

func (b *Backend) Prepare(ctx context.Context) error {
	if _, err := exec.LookPath("dockerd"); err != nil {
		return errors.New("dockerd не найден: установите Docker Engine (apt/dnf install docker-ce)")
	}
	return nil
}

func (b *Backend) Start(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "systemctl", "start", "docker").Run(); err != nil {
		return fmt.Errorf("systemctl start docker: %w", err)
	}
	// Дожидаемся готовности сокета.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("dockerd не поднялся за 30 секунд")
}

func (b *Backend) Stop(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "systemctl", "stop", "docker").Run(); err != nil {
		return fmt.Errorf("systemctl stop docker: %w", err)
	}
	return nil
}

func (b *Backend) Status(ctx context.Context) (backend.Status, error) {
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return backend.Status{Running: false, State: "stopped", Detail: err.Error()}, nil
	}
	conn.Close()
	return backend.Status{Running: true, State: "running", Detail: "нативный dockerd"}, nil
}

func (b *Backend) GuestSocket(ctx context.Context) (net.Conn, error) {
	// Нативный режим: прокси не нужен, но интерфейс единый.
	return net.Dial("unix", socketPath)
}

func (b *Backend) Exec(ctx context.Context, cmd ...string) ([]byte, error) {
	return exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
}

func init() {
	backend.Register("linux", New)
}
