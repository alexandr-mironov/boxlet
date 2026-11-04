package qemu

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/certs"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
)

func testBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	// Свободный порт, на котором никто не слушает.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DockerPort = ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	b := &Backend{cfg: cfg, layout: paths.New(root)}
	bundle, err := certs.Ensure(b.layout.CertsDir)
	if err != nil {
		t.Fatal(err)
	}
	if b.tlsConfig, err = bundle.ClientConfig(); err != nil {
		t.Fatal(err)
	}
	return b
}

// Если qemu умер во время загрузки, ждать весь bootTimeout нельзя.
func TestWaitDockerStopsWhenQemuExits(t *testing.T) {
	b := testBackend(t)
	exited := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(exited) })

	began := time.Now()
	err := b.waitDocker(context.Background(), exited, time.Minute)
	if !errors.Is(err, errQemuExited) {
		t.Fatalf("ошибка %v, ожидалась errQemuExited", err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("waitDocker заметил выход qemu только через %s", d)
	}
}

func TestWaitDockerHonorsCancel(t *testing.T) {
	b := testBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := b.waitDocker(ctx, make(chan struct{}), time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка %v, ожидалась context.Canceled", err)
	}
}

// Stop во время Start (до запуска qemu) прерывает Start,
// и повторный Start снова возможен.
func TestStopCancelsPendingStart(t *testing.T) {
	b := testBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.cancelStart = cancel
	b.setState("starting", "подготовка")

	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("Stop не отменил идущий Start")
	}
	st, _ := b.Status(context.Background())
	if st.State != "stopped" {
		t.Fatalf("состояние %q после Stop", st.State)
	}
}

// pid из pidfile, доставшийся не-qemu процессу, не должен считаться VM.
func TestOrphanProcessIgnoresForeignPID(t *testing.T) {
	b := testBackend(t)
	if err := os.MkdirAll(filepath.Dir(b.pidPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(b.pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if p := b.orphanProcess(); p != nil {
		t.Fatalf("процесс теста (pid %d) принят за qemu", p.Pid)
	}
	os.WriteFile(b.pidPath(), []byte("999999"), 0o644)
	if p := b.orphanProcess(); p != nil {
		t.Fatal("несуществующий pid принят за qemu")
	}
}
