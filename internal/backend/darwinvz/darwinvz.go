//go:build darwin

// Package darwinvz реализует бэкенд на Virtualization.framework (macOS).
// VM-процесс — helper-бинарь boxlet-vm (Swift, см. vmhelper/), которым
// демон управляет как дочерним процессом.
//
// Отличия от qemu-бэкенда:
//   - сеть: NAT через vmnet; IP гостя доступен хосту напрямую,
//     docker API слушается на guestIP:2375 (без hostfwd);
//   - virtiofs-шары каталогов хоста (bind-mount в контейнеры);
//   - Rosetta для запуска linux/amd64 образов на Apple Silicon.
package darwinvz

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/guest"
	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/vmnet"
)

// bootTimeout — первая загрузка ставит пакеты из сети.
const bootTimeout = 10 * time.Minute

type Backend struct {
	cfg    *config.Config
	layout paths.Layout

	mu      sync.Mutex
	status  backend.Status
	cmd     *exec.Cmd
	guestIP string
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

func (b *Backend) Name() string { return "darwin-vz" }

func (b *Backend) diskPath() string { return filepath.Join(b.layout.DataDir, "boxlet-vz.raw") }
func (b *Backend) pidPath() string  { return filepath.Join(b.layout.Root, "run", "boxlet-vm.pid") }
func (b *Backend) serialLog() string {
	return filepath.Join(b.layout.LogsDir, "vm-serial.log")
}

// helperPath ищет boxlet-vm рядом с текущим исполняемым файлом.
func helperPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), "boxlet-vm")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("helper boxlet-vm не найден по пути %s; соберите: make vm-helper", path)
	}
	return path, nil
}

// Prepare скачивает netboot-файлы Alpine, проверяет helper
// и создаёт raw-диск под данные dockerd.
func (b *Backend) Prepare(ctx context.Context) error {
	if _, err := helperPath(); err != nil {
		return err
	}
	if _, err := guest.Ensure(ctx, b.layout.ImagesDir, b.cfg.AlpineRelease, b.cfg.AlpineMirror, os.Stdout); err != nil {
		return err
	}
	if err := b.ensureDisk(); err != nil {
		return err
	}
	return b.ensureMAC()
}

// ensureDisk создаёт разреженный raw-диск (VZ не читает qcow2).
func (b *Backend) ensureDisk() error {
	if _, err := os.Stat(b.diskPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(b.layout.DataDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(b.diskPath())
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(int64(b.cfg.DiskGiB) << 30)
}

// ensureMAC генерирует и сохраняет локально-администрируемый MAC.
func (b *Backend) ensureMAC() error {
	if b.cfg.MAC != "" {
		return nil
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	buf[0] = (buf[0] | 0x02) & 0xfe // locally administered, unicast
	b.cfg.MAC = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5])
	return b.cfg.Save()
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

	apkovl, err := guest.BuildApkovl(b.cfg.AlpineMirror, b.cfg.AlpineRelease, b.cfg.Mounts, b.cfg.Rosetta)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}
	arch, err := guest.AlpineArch("arm64")
	if err != nil {
		return err
	}
	assetsDir := filepath.Join(b.layout.ImagesDir, "alpine", arch)
	// VZ принимает только несжатое ядро Image; Alpine отдаёт zboot-обёртку.
	kernel, err := guest.EnsureVMLinux(assetsDir)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}
	assets := &guest.Assets{
		Kernel:  kernel,
		Initrd:  filepath.Join(assetsDir, "initramfs-virt"),
		Modloop: filepath.Join(assetsDir, "modloop-virt"),
	}

	netboot, err := guest.ServeNetboot(apkovl)
	if err != nil {
		b.setState("error", err.Error())
		return err
	}

	helper, err := helperPath()
	if err != nil {
		netboot.Close()
		b.setState("error", err.Error())
		return err
	}

	args := []string{
		"--kernel", assets.Kernel,
		"--initrd", assets.Initrd,
		// console=hvc0 — серийная консоль virtio-console;
		// apkovl по HTTP с адреса шлюза vmnet (см. vmnet.GatewayIP).
		"--cmdline", "console=hvc0 ip=dhcp apkovl=" + netboot.ApkovlURLFor(vmnet.GatewayIP()),
		"--disk", assets.Modloop + ":ro",
		"--disk", b.diskPath(),
		"--cpus", strconv.Itoa(b.cfg.CPUs),
		"--memory-mib", strconv.Itoa(b.cfg.MemoryMiB),
		"--mac", b.cfg.MAC,
		"--serial", b.serialLog(),
	}
	for i, m := range b.cfg.Mounts {
		args = append(args, "--share", fmt.Sprintf("share%d=%s", i, m))
	}
	if b.cfg.Rosetta {
		args = append(args, "--rosetta")
	}

	if err := os.MkdirAll(b.layout.LogsDir, 0o755); err != nil {
		netboot.Close()
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(b.layout.LogsDir, "boxlet-vm.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		netboot.Close()
		return err
	}

	cmd := exec.Command(helper, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		netboot.Close()
		b.setState("error", err.Error())
		return fmt.Errorf("запустить boxlet-vm: %w", err)
	}
	os.WriteFile(b.pidPath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)

	b.mu.Lock()
	b.cmd = cmd
	b.setStateLocked("starting", "VM загружается, ищу IP гостя")
	b.mu.Unlock()

	go func() {
		cmd.Wait()
		logFile.Close()
		netboot.Close()
		os.Remove(b.pidPath())
		b.mu.Lock()
		b.cmd = nil
		b.guestIP = ""
		if b.status.State != "stopping" {
			b.setStateLocked("stopped", "boxlet-vm завершился")
		}
		b.mu.Unlock()
	}()

	// Ждём DHCP-lease гостя и готовности dockerd.
	ip, err := b.waitGuestIP(ctx, 5*time.Minute)
	if err != nil {
		b.Stop(context.Background())
		b.setState("error", err.Error())
		return fmt.Errorf("IP гостевой ОС: %w", err)
	}
	b.mu.Lock()
	b.guestIP = ip
	b.mu.Unlock()

	b.setState("starting", "жду dockerd (первая загрузка ставит пакеты, до нескольких минут)")
	if err := waitDocker(ctx, net.JoinHostPort(ip, strconv.Itoa(b.cfg.DockerPort)), bootTimeout); err != nil {
		b.Stop(context.Background())
		b.setState("error", err.Error())
		return fmt.Errorf("dockerd не поднялся: %w (см. %s)", err, b.serialLog())
	}

	b.setState("running", fmt.Sprintf("dockerd готов (%s:%d)", ip, b.cfg.DockerPort))
	return nil
}

// waitGuestIP опрашивает /var/db/dhcpd_leases, пока vmnet не выдаст
// гостю адрес по нашему MAC.
func (b *Backend) waitGuestIP(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ip, err := vmnet.LookupLease(b.cfg.MAC); err == nil && ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("lease не найден за %s", timeout)
}

// waitDocker ждёт ответа dockerd по TCP.
func waitDocker(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
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
	b.setStateLocked("stopping", "сбрасываю данные на диск (sync)")
	proc := b.cmd.Process
	ip := b.guestIP
	b.mu.Unlock()

	// sync гостевой ФС перед остановкой, чтобы не потерять данные dockerd.
	if ip != "" {
		if err := backend.SyncGuest(net.JoinHostPort(ip, strconv.Itoa(b.cfg.DockerPort))); err != nil {
			log.Printf("boxlet/darwin-vz: sync гостевой ОС не удался: %v", err)
		}
	}

	proc.Signal(syscall.SIGTERM) // helper штатно остановит VM
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			b.setState("stopped", "остановлен")
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

// GuestIP возвращает текущий IP гостевой ОС: vmnet NAT даёт
// маршрутизируемый с хоста адрес, поэтому порты гостя доступны напрямую.
// Реализует backend.Porter — используется форвардером портов.
func (b *Backend) GuestIP() (string, error) {
	b.mu.Lock()
	ip := b.guestIP
	b.mu.Unlock()
	if ip == "" {
		if ip2, err := vmnet.LookupLease(b.cfg.MAC); err == nil {
			ip = ip2
		}
	}
	if ip == "" {
		return "", errors.New("VM не запущена или IP гостя неизвестен")
	}
	return ip, nil
}

// GuestSocket подключается к dockerd напрямую по IP гостя,
// hostfwd не нужен.
func (b *Backend) GuestSocket(ctx context.Context) (net.Conn, error) {
	ip, err := b.GuestIP()
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(b.cfg.DockerPort)))
}

// Exec в M2 не поддержан; обходной путь — privileged-контейнер + nsenter.
func (b *Backend) Exec(ctx context.Context, cmd ...string) ([]byte, error) {
	return nil, errors.New("shell в VM появится позже; обходной путь: docker run -it --rm --privileged --pid=host alpine nsenter -t 1 -m sh")
}

// running сообщает, жив ли helper-процесс.
func (b *Backend) running() bool {
	if b.cmd != nil && b.cmd.Process != nil {
		return b.cmd.Process.Signal(syscall.Signal(0)) == nil
	}
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
	backend.Register("darwin-vz", New)
}
