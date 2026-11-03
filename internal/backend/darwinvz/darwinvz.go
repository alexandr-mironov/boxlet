// Package darwinvz реализует бэкенд на Virtualization.framework (macOS).
//
// Go не имеет биндингов к VZ, поэтому VM-процесс — helper-бинарь
// boxlet-vm (Swift), которым boxletd управляет как дочерним процессом.
// Каналы: vsock (гостевой агент, проксирование docker-сокета),
// virtiofs (шаринг ~/projects), Rosetta (linux/amd64).
//
// Появится в M2 (см. docs/roadmap.md).
package darwinvz

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/alexandr-mironov/boxlet/internal/backend"
)

var errNotImplemented = errors.New("darwin-vz-бэкенд появится в M2; пока используйте backend=qemu")

type Backend struct {
	mu     sync.Mutex
	status backend.Status
}

func New() backend.Backend {
	return &Backend{status: backend.Status{State: "stopped", Detail: "не запускался"}}
}

func (b *Backend) Name() string { return "darwin-vz" }

func (b *Backend) Prepare(ctx context.Context) error                  { return errNotImplemented }
func (b *Backend) Start(ctx context.Context) error                    { return errNotImplemented }
func (b *Backend) Stop(ctx context.Context) error                     { return errNotImplemented }
func (b *Backend) GuestSocket(ctx context.Context) (net.Conn, error)  { return nil, errNotImplemented }
func (b *Backend) Exec(ctx context.Context, cmd ...string) ([]byte, error) {
	return nil, errNotImplemented
}

func (b *Backend) Status(ctx context.Context) (backend.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status, nil
}

func init() {
	backend.Register("darwin-vz", New)
}
