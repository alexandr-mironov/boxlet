package qemu

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// TestMain: в роли фейкового qemu тестовый бинарь просто спит
// (см. fakeQemu).
func TestMain(m *testing.M) {
	if os.Getenv("BOXLET_FAKE_QEMU") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeQemu запускает процесс с именем qemu-system-fake (копия тестового
// бинаря) и заданными аргументами — для проверки ownsProcess.
func fakeQemu(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "qemu-system-fake")
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "BOXLET_FAKE_QEMU=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	// ps должен увидеть уже запущенный процесс с нашим именем.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("ps", "-p", strconv.Itoa(cmd.Process.Pid), "-o", "comm=").Output()
		if strings.Contains(string(out), "qemu-system-fake") {
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("фейковый qemu не появился в ps")
	return nil
}

// Чужой qemu того же пользователя не должен приниматься за VM инстанса.
func TestOwnsProcessRequiresInstancePaths(t *testing.T) {
	b := testBackend(t)

	foreign := fakeQemu(t, "-drive", "file=/tmp/other.qcow2,if=virtio", "-pidfile", "/tmp/other.pid")
	if b.ownsProcess(foreign.Process.Pid) {
		t.Fatal("чужой qemu принят за VM инстанса")
	}

	ours := fakeQemu(t, "-drive", "file="+b.diskPath()+",if=virtio,format=qcow2", "-pidfile", b.pidPath())
	if !b.ownsProcess(ours.Process.Pid) {
		t.Fatal("qemu инстанса не опознан")
	}
}

// Таймаут загрузки не должен превращаться в «запуск прерван остановкой»:
// очистка после неудачной загрузки не отменяет контекст Start.
func TestFailBootKeepsStartContext(t *testing.T) {
	b := testBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.cancelStart = cancel

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	b.proc, b.exited = cmd.Process, exited
	go func() {
		cmd.Wait()
		b.processExited(cmd.Process, exited)
	}()

	b.failBoot(cmd.Process, exited)
	if ctx.Err() != nil {
		t.Fatal("failBoot отменил контекст Start")
	}
	select {
	case <-exited:
	default:
		t.Fatal("VM не остановлена")
	}
}
