// Package qemu реализует бэкенд на базе QEMU: diskless Alpine Linux
// (netboot ядро/initramfs + modloop read-only диском + apkovl-оверлей)
// с dockerd внутри.
//
// Поток загрузки:
//  1. Prepare скачивает vmlinuz/initramfs/modloop, создаёт data-диск.
//  2. Start поднимает временный HTTP-сервер с apkovl-оверлеем и запускает
//     qemu с kernel cmdline ip=dhcp apkovl=...; modloop подключён
//     read-only virtio-диском — из него гостевая ОС берёт модули ядра.
//  3. Гостевая ОС применяет оверлей, local.d-скрипт ставит docker
//     и запускает dockerd на unix-сокете и TCP с tlsverify.
//  4. Start дожидается ответа dockerd на 127.0.0.1:<docker_port> по TLS.
//
// Остановка — штатное выключение гостевой ОС (poweroff через init гостя);
// SIGTERM/SIGKILL qemu — только запасной путь.
package qemu

import (
	"context"
	"crypto/tls"
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
	"github.com/alexandr-mironov/boxlet/internal/certs"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/guest"
	"github.com/alexandr-mironov/boxlet/internal/paths"
)

const (
	// bootTimeout — сколько ждём готовности dockerd (первая загрузка
	// ставит пакеты из сети и может занять несколько минут).
	bootTimeout = 10 * time.Minute
	// adoptTimeout — сколько ждём dockerd в VM, оставшейся от прошлого
	// запуска демона, прежде чем признать её неработоспособной.
	adoptTimeout = 30 * time.Second
	// powerdownTimeout — сколько ждём штатного выключения гостевой ОС.
	powerdownTimeout = 30 * time.Second
	// termTimeout — сколько ждём выхода qemu после SIGTERM.
	termTimeout = 15 * time.Second
)

// errQemuExited — qemu завершился, пока мы ждали dockerd.
var errQemuExited = errors.New("qemu завершился")

type Backend struct {
	cfg    *config.Config
	layout paths.Layout

	mu     sync.Mutex
	status backend.Status
	// proc — процесс qemu: дочерний или подхваченный после перезапуска
	// демона. exited закрывается, когда он завершился.
	proc   *os.Process
	exited chan struct{}
	// cancelStart прерывает идущий Start (вызывается из Stop).
	cancelStart context.CancelFunc
	tlsConfig   *tls.Config
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

// Prepare скачивает netboot-файлы Alpine, создаёт data-диск
// и TLS-сертификаты для dockerd.
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
	if _, err := certs.Ensure(b.layout.CertsDir); err != nil {
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

// Start поднимает VM и ждёт dockerd. Идемпотентен для уже работающей VM.
// Stop во время Start прерывает загрузку.
func (b *Backend) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.cancelStart != nil {
		b.mu.Unlock()
		return errors.New("запуск уже идёт")
	}
	if b.proc != nil && b.status.State == "running" {
		b.mu.Unlock()
		return nil
	}
	if b.proc != nil {
		b.mu.Unlock()
		return errors.New("VM ещё не остановилась")
	}
	ctx, cancel := context.WithCancel(ctx)
	b.cancelStart = cancel
	b.setStateLocked("starting", "подготовка гостевой ОС")
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.cancelStart = nil
		b.mu.Unlock()
		cancel()
	}()

	err := b.start(ctx)
	if err == nil {
		b.setState("running", fmt.Sprintf("dockerd готов (127.0.0.1:%d, TLS)", b.cfg.DockerPort))
		return nil
	}
	if ctx.Err() != nil {
		// Прервано через Stop: состояние выставляет он.
		return errors.New("запуск прерван остановкой VM")
	}
	b.setState("error", err.Error())
	return err
}

func (b *Backend) start(ctx context.Context) error {
	if err := b.Prepare(ctx); err != nil {
		return err
	}
	bundle, err := certs.Ensure(b.layout.CertsDir)
	if err != nil {
		return err
	}
	tlsConfig, err := bundle.ClientConfig()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.tlsConfig = tlsConfig
	b.mu.Unlock()

	// VM могла остаться от прошлого запуска демона (его убили, а qemu
	// продолжил работать): подхватываем её, если dockerd в ней отвечает,
	// иначе выключаем и загружаем заново.
	if adopted, err := b.adoptOrphan(ctx); err != nil || adopted {
		return err
	}

	// qemu-бэкенд: без virtiofs-шар хоста (нужен virtiofsd — в roadmap)
	// и без Rosetta (это привилегия darwin-vz).
	agent, _ := guest.AgentBinary(runtime.GOARCH)
	token, err := guest.AgentToken(b.layout.Root)
	if err != nil {
		return err
	}
	apkovl, err := guest.BuildApkovl(b.cfg.AlpineMirror, b.cfg.AlpineRelease, nil, false, agent, token, bundle)
	if err != nil {
		return err
	}
	assets, err := guest.Ensure(ctx, b.layout.ImagesDir, b.cfg.AlpineRelease, b.cfg.AlpineMirror, io.Discard)
	if err != nil {
		return err
	}

	// Сервер с apkovl нужен только на время загрузки: в оверлее лежит
	// TLS-ключ dockerd, держать его доступным дольше незачем.
	netboot, err := guest.ServeNetboot("127.0.0.1", apkovl)
	if err != nil {
		return err
	}
	defer netboot.Close()

	args, err := b.qemuArgs(assets, netboot)
	if err != nil {
		return err
	}

	qbin, _ := qemuBinary()
	cmd := exec.Command(qbin, args...)
	// Логи qemu и серийной консоли гостевой ОС.
	if err := os.MkdirAll(b.layout.LogsDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(b.layout.LogsDir, "qemu.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("запустить qemu: %w", err)
	}

	// Регистрируем процесс под мьютексом: Stop, пришедший до этого
	// момента, уже отменил ctx и процесс не увидит — гасим его сами.
	exited := make(chan struct{})
	b.mu.Lock()
	if ctx.Err() != nil {
		b.mu.Unlock()
		cmd.Process.Kill()
		cmd.Wait()
		logFile.Close()
		return ctx.Err()
	}
	b.proc, b.exited = cmd.Process, exited
	b.setStateLocked("starting", "VM загружается, dockerd ещё не готов")
	b.mu.Unlock()

	go func() {
		cmd.Wait()
		logFile.Close()
		b.processExited(cmd.Process, exited)
	}()

	b.setState("starting", "жду dockerd (первая загрузка ставит пакеты, до нескольких минут)")
	err = b.waitDocker(ctx, exited, bootTimeout)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errQemuExited):
		return fmt.Errorf("qemu завершился во время загрузки (см. %s и %s)",
			filepath.Join(b.layout.LogsDir, "qemu.log"), b.serialLog())
	default:
		b.Stop(context.Background())
		return fmt.Errorf("dockerd не поднялся: %w (см. %s)", err, b.serialLog())
	}
}

// adoptOrphan ищет qemu, оставшийся от прошлого запуска демона.
// Если dockerd в нём отвечает — подхватывает VM (adopted=true),
// иначе выключает её, чтобы загрузить заново.
func (b *Backend) adoptOrphan(ctx context.Context) (adopted bool, err error) {
	proc := b.orphanProcess()
	if proc == nil {
		return false, nil
	}
	log.Printf("boxlet/qemu: найдена VM от прошлого запуска (pid %d)", proc.Pid)

	exited := make(chan struct{})
	b.mu.Lock()
	if ctx.Err() != nil {
		b.mu.Unlock()
		return false, ctx.Err()
	}
	b.proc, b.exited = proc, exited
	b.setStateLocked("starting", fmt.Sprintf("подключаюсь к VM от прошлого запуска (pid %d)", proc.Pid))
	b.mu.Unlock()

	// Процесс не наш дочерний — Wait недоступен, следим через signal 0.
	go func() {
		for proc.Signal(syscall.Signal(0)) == nil {
			time.Sleep(time.Second)
		}
		b.processExited(proc, exited)
	}()

	err = b.waitDocker(ctx, exited, adoptTimeout)
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case errors.Is(err, errQemuExited):
		return false, nil
	}
	log.Printf("boxlet/qemu: dockerd в старой VM не отвечает (%v), перезапускаю", err)
	b.shutdown(ctx, proc, exited)
	return false, ctx.Err()
}

// orphanProcess возвращает живой qemu из pidfile или nil.
// Имя процесса проверяется: pid мог достаться другой программе.
func (b *Backend) orphanProcess() *os.Process {
	data, err := os.ReadFile(b.pidPath())
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.Signal(0)) != nil {
		return nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil || !strings.HasPrefix(filepath.Base(strings.TrimSpace(string(out))), "qemu-system") {
		return nil
	}
	return proc
}

// processExited снимает регистрацию завершившегося qemu.
func (b *Backend) processExited(proc *os.Process, exited chan struct{}) {
	b.mu.Lock()
	if b.proc == proc {
		b.proc, b.exited = nil, nil
		if b.status.State != "stopping" {
			b.setStateLocked("stopped", "qemu завершился")
		}
	}
	b.mu.Unlock()
	close(exited)
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

	// Проброс портов: dockerd (TLS) + гостевой агент (boxlet shell)
	// + пользовательские из конфига.
	fwds := []string{
		fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", b.cfg.DockerPort, guest.DockerPort),
		fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", guest.AgentPort, guest.AgentPort),
	}
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
	}
	return args, nil
}

// dialDocker открывает TLS-соединение с dockerd через NAT-проброс qemu.
func (b *Backend) dialDocker(ctx context.Context) (net.Conn, error) {
	b.mu.Lock()
	cfg := b.tlsConfig
	b.mu.Unlock()
	if cfg == nil {
		return nil, errors.New("TLS-сертификаты dockerd ещё не загружены")
	}
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 3 * time.Second},
		Config:    cfg,
	}
	return d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", b.cfg.DockerPort))
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
// Прерывается отменой ctx и завершением qemu (exited).
func (b *Backend) waitDocker(ctx context.Context, exited <-chan struct{}, timeout time.Duration) error {
	client := backend.DockerClient(b.dialDocker, 5*time.Second)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://boxlet/_ping", nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return errQemuExited
		case <-deadline.C:
			return fmt.Errorf("таймаут %s", timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

// Stop останавливает VM; идущий Start при этом прерывается.
func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.cancelStart != nil {
		b.cancelStart()
	}
	proc, exited := b.proc, b.exited
	if proc == nil {
		b.setStateLocked("stopped", "остановлен")
		b.mu.Unlock()
		return nil
	}
	b.setStateLocked("stopping", "останавливаю VM")
	b.mu.Unlock()

	return b.shutdown(ctx, proc, exited)
}

// shutdown выключает VM: sync данных, штатное выключение гостевой ОС
// (см. backend.PowerOffGuest), и только если гость не отвечает —
// SIGTERM/SIGKILL qemu.
func (b *Backend) shutdown(ctx context.Context, proc *os.Process, exited <-chan struct{}) error {
	// sync до всего остального: если штатное выключение не сработает,
	// SIGTERM хотя бы не потеряет данные из page cache гостя.
	b.setState("stopping", "сбрасываю данные на диск (sync)")
	if err := backend.SyncGuest(b.dialDocker); err != nil {
		log.Printf("boxlet/qemu: sync гостевой ОС не удался: %v", err)
	}

	b.setState("stopping", "выключаю гостевую ОС")
	if err := backend.PowerOffGuest(b.dialDocker); err != nil {
		log.Printf("boxlet/qemu: штатное выключение не удалось: %v", err)
	} else if ok, err := waitExit(ctx, exited, powerdownTimeout); err != nil {
		return err
	} else if ok {
		b.setState("stopped", "остановлен")
		return nil
	} else {
		log.Printf("boxlet/qemu: гостевая ОС не выключилась за %s, SIGTERM", powerdownTimeout)
	}

	proc.Signal(syscall.SIGTERM)
	if ok, err := waitExit(ctx, exited, termTimeout); err != nil {
		return err
	} else if ok {
		b.setState("stopped", "остановлен (SIGTERM)")
		return nil
	}
	proc.Signal(syscall.SIGKILL)
	if _, err := waitExit(ctx, exited, 5*time.Second); err != nil {
		return err
	}
	b.setState("stopped", "остановлен принудительно (SIGKILL)")
	return nil
}

// waitExit ждёт закрытия exited не дольше timeout.
func waitExit(ctx context.Context, exited <-chan struct{}, timeout time.Duration) (bool, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-exited:
		return true, nil
	case <-t.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (b *Backend) Status(ctx context.Context) (backend.Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.status
	st.Running = b.proc != nil && st.State == "running"
	return st, nil
}

// GuestSocket подключается к dockerd через NAT-проброс по TLS.
func (b *Backend) GuestSocket(ctx context.Context) (net.Conn, error) {
	b.mu.Lock()
	running := b.proc != nil
	b.mu.Unlock()
	if !running {
		return nil, errors.New("VM не запущена")
	}
	// tls.Dialer возвращает соединение после рукопожатия — это важно
	// для прокси: tls.Conn допускает CloseWrite только после handshake.
	// CloseWrite у tls.Conn шлёт close_notify, и dockerd видит его как EOF,
	// так что half-close через прокси работает и поверх TLS.
	return b.dialDocker(ctx)
}

// AgentAddr — адрес гостевого агента через hostfwd. Реализует backend.Sheller.
func (b *Backend) AgentAddr() (string, error) {
	b.mu.Lock()
	running := b.proc != nil
	b.mu.Unlock()
	if !running {
		return "", errors.New("VM не запущена")
	}
	return fmt.Sprintf("127.0.0.1:%d", guest.AgentPort), nil
}

// Exec выполняет команду в гостевой ОС через boxlet-agent.
func (b *Backend) Exec(ctx context.Context, cmd ...string) ([]byte, error) {
	addr, err := b.AgentAddr()
	if err != nil {
		return nil, err
	}
	token, err := guest.AgentToken(b.layout.Root)
	if err != nil {
		return nil, err
	}
	return backend.AgentExec(ctx, addr, token, cmd)
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
