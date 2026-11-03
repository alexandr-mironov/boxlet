// Package qemu реализует бэкенд на базе QEMU: diskless Alpine Linux
// (netboot ядро/initramfs + alpine-virt ISO как источник modloop
// + apkovl-оверлей) с dockerd внутри.
//
// Поток загрузки:
//  1. Prepare скачивает vmlinuz/initramfs и alpine-virt ISO, создаёт data-диск.
//  2. Start поднимает временный HTTP-сервер с apkovl-оверлеем и запускает
//     qemu с kernel cmdline ip=dhcp apkovl=..., ISO подключён read-only —
//     initramfs монтирует с него modloop (модули ядра, в т.ч. overlayfs).
//  3. Гостевая ОС применяет оверлей, local.d-скрипт ставит docker
//     и запускает dockerd на unix+tcp.
//  4. Start дожидается ответа dockerd на 127.0.0.1:2375.
package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/guest"
	"github.com/alexandr-mironov/boxlet/internal/paths"
)

// bootTimeout — сколько ждём готовности dockerd (первая загрузка
// ставит пакеты из сети и может занять несколько минут).
const bootTimeout = 10 * time.Minute

type Backend struct {
	cfg    *config.Config
	layout paths.Layout

	mu     sync.Mutex
	status backend.Status
	cmd    *exec.Cmd
}

func New() backend.Backend {
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.Default()
	}
	root, _ := config.Dir()
	return &Backend{
		cfg:    cfg,
		layout: paths.New(root),
		status: backend.Status{State: "stopped", Detail: "не запускался"},
	}
}

func (b *Backend) Name() string { return "qemu" }

// qemuBinary возвращает имя бинаря qemu под архитектуру хоста.
func qemuBinary() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "qemu-system-aarch64", nil
	case "amd64":
		return "qemu-system-x86_64", nil
	default:
		return "", fmt.Errorf("архитектура %s не поддерживается", runtime.GOARCH)
	}
}

func (b *Backend) diskPath() string { return filepath.Join(b.layout.DataDir, "boxlet.qcow2") }
func (b *Backend) pidPath() string  { return filepath.Join(b.layout.Root, "run", "qemu.pid") }
func (b *Backend) serialLog() string {
	return filepath.Join(b.layout.LogsDir, "qemu-serial.log")
}

// Prepare скачивает netboot-файлы Alpine и создаёт data-диск.
func (b *Backend) Prepare(ctx context.Context) error {
	qbin, err := qemuBinary()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath(qbin); err != nil {
		return fmt.Errorf("%s не найден: установите qemu (macOS: brew install qemu; Debian/Ubuntu: apt install qemu-system)", qbin)
	}
	if _, err := guest.Ensure(ctx, b.layout.ImagesDir, b.cfg.AlpineRelease, b.cfg.AlpineMirror, os.Stdout); err != nil {
		return err
	}
	return b.ensureDisk()
}

// ensureDisk создаёт qcow2-диск под данные dockerd, если его ещё нет.
func (b *Backend) ensureDisk() error {
	if _, err := os.Stat(b.diskPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(b.layout.DataDir, 0o755); err != nil {
		return err
	}
	size := fmt.Sprintf("%dG", b.cfg.DiskGiB)
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", b.diskPath(), size).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %w (%s)", err, out)
	}
	return nil
}

func (b *Backend) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running() {
		b.mu.Unlock()
		return errors.New("уже запущен")
	}
	b.setStateLocked("starting", "подготовка гостевой ОС")
	b.mu.Unlock()

	if err := b.Prepare(ctx); err != nil {
		b.setState("error", err.Error())
		return err
	}

	apkovl, err := guest.BuildApkovl(b.cfg.AlpineMirror, b.cfg.AlpineRelease)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}
	assets, err := guest.Ensure(ctx, b.layout.ImagesDir, b.cfg.AlpineRelease, b.cfg.AlpineMirror, io.Discard)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}

	netboot, err := guest.ServeNetboot(apkovl)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}

	args, err := b.qemuArgs(assets, netboot)
	if err != nil {
		netboot.Close()
		b.setState("error", err.Error())
		return err
	}

	qbin, _ := qemuBinary()
	cmd := exec.Command(qbin, args...)
	// Логи qemu и серийной консоли гостевой ОС.
	if err := os.MkdirAll(b.layout.LogsDir, 0o755); err != nil {
		netboot.Close()
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(b.layout.LogsDir, "qemu.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		netboot.Close()
		return err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		netboot.Close()
		b.setState("error", err.Error())
		return fmt.Errorf("запустить qemu: %w", err)
	}

	b.mu.Lock()
	b.cmd = cmd
	b.setStateLocked("starting", "VM загружается, dockerd ещё не готов")
	b.mu.Unlock()

	// Освобождаем ресурсы, когда qemu завершится.
	go func() {
		cmd.Wait()
		logFile.Close()
		netboot.Close()
		b.mu.Lock()
		b.cmd = nil
		if b.status.State != "stopping" {
			b.setStateLocked("stopped", "qemu завершился")
		}
		b.mu.Unlock()
	}()

	// Ждём готовности dockerd за NAT-пробросом.
	b.setState("starting", "жду dockerd (первая загрузка ставит пакеты, до нескольких минут)")
	if err := b.waitDocker(ctx, bootTimeout); err != nil {
		b.Stop(context.Background())
		b.setState("error", err.Error())
		return fmt.Errorf("dockerd не поднялся: %w (см. %s)", err, b.serialLog())
	}

	b.setState("running", fmt.Sprintf("dockerd готов (127.0.0.1:%d)", b.cfg.DockerPort))
	return nil
}

// qemuArgs собирает аргументы командной строки qemu.
func (b *Backend) qemuArgs(assets *guest.Assets, nb *guest.NetbootServer) ([]string, error) {
	var machine, cpu, console string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		machine, cpu, console = "virt,accel=hvf", "host", "console=ttyAMA0"
	case "darwin/amd64":
		machine, cpu, console = "pc,accel=hvf", "host", "console=ttyS0"
	case "linux/arm64":
		machine, cpu, console = "virt,accel=kvm", "host", "console=ttyAMA0"
	case "linux/amd64":
		machine, cpu, console = "pc,accel=kvm", "host", "console=ttyS0"
	default:
		return nil, fmt.Errorf("платформа %s/%s не поддерживается", runtime.GOOS, runtime.GOARCH)
	}

	kargs := strings.Join([]string{
		console,
		"ip=dhcp",
		"apkovl=" + nb.ApkovlURL(),
	}, " ")

	// Проброс портов: dockerd + пользовательские из конфига.
	fwds := []string{fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", b.cfg.DockerPort, b.cfg.DockerPort)}
	fwds = append(fwds, b.forwardedPorts()...)
	netdev := "user,id=n0," + strings.Join(fwds, ",")

	args := []string{
		"-machine", machine,
		"-cpu", cpu,
		"-smp", strconv.Itoa(b.cfg.CPUs),
		"-m", strconv.Itoa(b.cfg.MemoryMiB),
		"-kernel", assets.Kernel,
		"-initrd", assets.Initrd,
		"-append", kargs,
		// modloop как read-only virtio-диск: гостевая ОС монтирует
		// его напрямую как squashfs и получает модули ядра (overlayfs).
		"-drive", "file=" + assets.Modloop + ",if=virtio,format=raw,readonly=on",
		"-drive", "file=" + b.diskPath() + ",if=virtio,format=qcow2",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:" + b.serialLog(),
		"-pidfile", b.pidPath(),
		// Монитор на unix-сокете: нужен для штатного выключения
		// (system_powerdown → ACPI → гостевой poweroff со sync дисков).
		"-monitor", "unix:" + b.monitorPath() + ",server,nowait",
	}
	return args, nil
}

func (b *Backend) monitorPath() string {
	return filepath.Join(b.layout.Root, "run", "qemu-monitor.sock")
}

// syncGuest сбрасывает гостевой page cache на диск через служебный
// контейнер boxlet-agent (создаётся bootstrap-скриптом из статического
// busybox). sync() в контейнере действует на всё ядро гостевой ОС.
// Best effort: при недоступности dockerd возвращает ошибку,
// вызывающий останавливает VM как есть.
func (b *Backend) syncGuest() error {
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				addr := fmt.Sprintf("127.0.0.1:%d", b.cfg.DockerPort)
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

// forwardedPorts разбирает ForwardedPorts из конфига в hostfwd-фрагменты.
// Привилегированные порты (<1024) пропускаются без root с предупреждением.
func (b *Backend) forwardedPorts() []string {
	var fwds []string
	for _, p := range b.cfg.ForwardedPorts {
		host, guestPort, ok := strings.Cut(p, ":")
		if !ok {
			continue
		}
		hp, err := strconv.Atoi(host)
		if err != nil {
			continue
		}
		if hp < 1024 && os.Geteuid() != 0 {
			fmt.Fprintf(os.Stderr, "boxlet: пропускаю проброс %s: порт <1024 требует root\n", p)
			continue
		}
		fwds = append(fwds, fmt.Sprintf("hostfwd=tcp:0.0.0.0:%d-:%s", hp, guestPort))
	}
	return fwds
}

// waitDocker ждёт ответа dockerd на проброшенном порту.
func (b *Backend) waitDocker(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", b.cfg.DockerPort)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			fmt.Fprintf(conn, "GET /_ping HTTP/1.1\r\nHost: boxlet\r\n\r\n")
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 256)
			n, _ := conn.Read(buf)
			conn.Close()
			if strings.Contains(string(buf[:n]), "200") {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("таймаут %s", timeout)
}

func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.cmd == nil || b.cmd.Process == nil {
		b.setStateLocked("stopped", "не запускался")
		b.mu.Unlock()
		return nil
	}
	b.setStateLocked("stopping", "останавливаю VM")
	proc := b.cmd.Process
	b.mu.Unlock()

	// Перед жёсткой остановкой сбрасываем гостевые данные на диск:
	// sync из контейнера действует на всё ядро гостевой ОС.
	// Без этого qemu SIGTERM теряет данные из page cache гостя.
	b.setState("stopping", "сбрасываю данные на диск (sync)")
	if err := b.syncGuest(); err != nil {
		log.Printf("boxlet/qemu: sync гостевой ОС не удался, останавливаю как есть: %v", err)
	}
	proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			b.setState("stopped", "остановлен пользователем")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	proc.Signal(syscall.SIGKILL)
	b.setState("stopped", "остановлен принудительно (SIGKILL)")
	return nil
}

func (b *Backend) Status(ctx context.Context) (backend.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.status
	st.Running = b.running() && st.State == "running"
	return st, nil
}

// GuestSocket подключается к dockerd через NAT-проброс.
func (b *Backend) GuestSocket(ctx context.Context) (net.Conn, error) {
	if !b.running() {
		return nil, errors.New("VM не запущена")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", b.cfg.DockerPort)
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// Exec в M1 не поддержан: shell в гостевую ОС появится с гостевым агентом (M2).
// Обходной путь: docker run -it --rm --privileged --pid=host alpine nsenter -t 1 -m sh
func (b *Backend) Exec(ctx context.Context, cmd ...string) ([]byte, error) {
	return nil, errors.New("shell в VM появится в M2; обходной путь: docker run -it --rm --privileged --pid=host alpine nsenter -t 1 -m sh")
}

// running сообщает, жив ли процесс qemu.
func (b *Backend) running() bool {
	if b.cmd != nil && b.cmd.Process != nil {
		return b.cmd.Process.Signal(syscall.Signal(0)) == nil
	}
	// Демон мог перезапуститься: проверяем pidfile.
	data, err := os.ReadFile(b.pidPath())
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func (b *Backend) setState(state, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setStateLocked(state, detail)
}

func (b *Backend) setStateLocked(state, detail string) {
	b.status = backend.Status{State: state, Detail: detail}
}

func init() {
	backend.Register("qemu", New)
}
