// boxletd — демон-суревизор: управляет бэкендом (VM/рантаймом),
// проксирует Docker API сокет на хост и отдаёт control API + веб-интерфейс.
package main

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
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/darwinvz"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/linux"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/qemu"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/dnsd"
	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/privhelper"
	"github.com/alexandr-mironov/boxlet/internal/proxy"
	"github.com/alexandr-mironov/boxlet/internal/webui"
)

type daemon struct {
	cfg    *config.Config
	root   string
	layout paths.Layout
	be     backend.Backend
	px     *proxy.Proxy

	startMu  sync.Mutex
	starting bool
	startErr error

	fwdMu     sync.Mutex
	fwd       *proxy.PortForwarder
	fwdCancel context.CancelFunc
	dnsCancel context.CancelFunc
	privCmd   *exec.Cmd // boxlet-priv: порты <1024
	privKey   string    // текущий делегированный список портов

	// opMu сериализует остановку и рестарт движка: два параллельных
	// stop/restart (трей шлёт POST, не дожидаясь ответа) не должны
	// гонять друг с другом внутри Backend.Stop.
	opMu sync.Mutex
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("boxletd: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root, err := config.Dir()
	if err != nil {
		return err
	}
	layout := paths.New(root)
	for _, d := range []string{layout.DataDir, layout.LogsDir, filepath.Dir(layout.DaemonSock)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	be, err := backend.New(cfg.Backend)
	if err != nil {
		return err
	}

	d := &daemon{cfg: cfg, root: root, layout: layout, be: be, px: proxy.New(be, layout.DockerSock)}

	// Control API + веб-интерфейс — поднимаем СРАЗУ, до запуска бэкенда:
	// загрузка VM занимает минуты, и CLI должен видеть прогресс.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/start", d.handleStart)
	mux.HandleFunc("/api/stop", d.handleStop)
	mux.HandleFunc("/api/restart", d.handleRestart)
	mux.HandleFunc("/api/metrics", d.handleMetrics)
	mux.HandleFunc("/api/logs", d.handleLogs)
	mux.HandleFunc("/api/containers", d.handleContainers)
	mux.Handle("/", webui.Handler())

	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.APIPort),
		Handler: mux,
	}
	// Синглтон: порт API занят — значит, демон уже запущен; второй
	// экземпляр создаст параллельную VM и рассинхрон состояния.
	apiLn, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("порт %s занят — boxletd уже запущен? (%w)", srv.Addr, err)
	}
	go func() {
		log.Printf("boxletd: веб-интерфейс на http://%s", srv.Addr)
		if err := srv.Serve(apiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("boxletd: http: %v", err)
		}
	}()

	// Автозапуск бэкенда в фоне.
	go d.startBackend(context.Background())

	// PID-файл, чтобы CLI мог проверить, жив ли демон.
	pidFile := filepath.Join(root, "run", "boxletd.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidFile)

	log.Printf("boxletd: запущен (backend=%s, socket=%s)", cfg.Backend, layout.DockerSock)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("boxletd: остановка…")
	srv.Close()
	d.stopPortForwarding()
	d.px.Stop()
	return d.be.Stop(context.Background())
}

// startBackend запускает бэкенд, прокси сокета и postStart-действия.
// Идемпотентен: параллельные вызовы не создают вторую VM.
func (d *daemon) startBackend(ctx context.Context) {
	d.startMu.Lock()
	if d.starting {
		d.startMu.Unlock()
		return
	}
	d.starting = true
	d.startErr = nil
	d.startMu.Unlock()

	defer func() {
		d.startMu.Lock()
		d.starting = false
		d.startMu.Unlock()
	}()

	if err := d.be.Start(ctx); err != nil {
		d.startMu.Lock()
		d.startErr = err
		d.startMu.Unlock()
		log.Printf("boxletd: бэкенд не запущен: %v", err)
		return
	}
	if err := d.px.Start(ctx); err != nil && !strings.Contains(err.Error(), "уже запущен") {
		d.startMu.Lock()
		d.startErr = err
		d.startMu.Unlock()
		log.Printf("boxletd: прокси сокета: %v", err)
		return
	}
	d.postStart(ctx)
	d.startPortForwarding()
	d.startDNS()
	log.Printf("boxletd: движок готов")
}

// startPortForwarding поднимает userspace-проброс портов на localhost,
// если бэкенд даёт прямой доступ к IP гостя (backend.Porter, darwin-vz).
// На qemu-бэкенде проброс делает сам qemu через hostfwd.
func (d *daemon) startPortForwarding() {
	p, ok := d.be.(backend.Porter)
	if !ok {
		return
	}
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	if d.fwdCancel != nil {
		return // уже запущен
	}
	fwd := proxy.NewPortForwarder(p.GuestIP)
	ctx, cancel := context.WithCancel(context.Background())
	d.fwd = fwd
	d.fwdCancel = cancel
	go d.portForwardLoop(ctx, fwd)
}

// startDNS поднимает DNS-сервер зоны boxlet.local (dnsd), отвечающий
// адресом гостевой ОС. Зона активируется файлом /etc/resolver/boxlet
// (boxlet dns install); без него сервер просто никому не нужен.
// На qemu-бэкенде IP гостя с хоста не маршрутизируется — DNS бессмыслен.
func (d *daemon) startDNS() {
	p, ok := d.be.(backend.Porter)
	if !ok {
		return
	}
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	if d.dnsCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.dnsCancel = cancel
	go func() {
		if err := dnsd.New(p.GuestIP).Run(ctx); err != nil {
			log.Printf("boxletd: dns: %v", err)
		}
	}()
}

func (d *daemon) stopPortForwarding() {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	if d.dnsCancel != nil {
		d.dnsCancel()
		d.dnsCancel = nil
	}
	if d.privCmd != nil && d.privCmd.Process != nil {
		d.privCmd.Process.Signal(syscall.SIGTERM)
		d.privCmd.Wait()
		d.privCmd = nil
	}
	d.privKey = ""
	if d.fwdCancel == nil {
		return
	}
	d.fwdCancel()
	d.fwdCancel = nil
	d.fwd = nil
}

// portForwardLoop следит за портами, которые нужно пробросить:
// статический список из конфига + порты, опубликованные запущенными
// контейнерами (опрос dockerd раз в 2 секунды; событийная модель
// по /events хрупче при перезапусках dockerd).
func (d *daemon) portForwardLoop(ctx context.Context, fwd *proxy.PortForwarder) {
	d.reconcilePorts(ctx, fwd)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fwd.Close()
			return
		case <-tick.C:
			d.reconcilePorts(ctx, fwd)
		}
	}
}

func (d *daemon) reconcilePorts(ctx context.Context, fwd *proxy.PortForwarder) {
	ports, err := proxy.ParsePortSpecs(d.cfg.ForwardedPorts)
	if err != nil {
		log.Printf("boxletd: конфиг forwarded_ports: %v", err)
		ports = map[int]int{}
	}

	// dockerd публикует порты контейнеров на 0.0.0.0 гостевой ОС;
	// пробрасываем каждый такой TCP-порт на 127.0.0.1 хоста.
	data, err := d.dockerAPI(ctx, http.MethodGet, "/containers/json", nil)
	if err == nil {
		var containers []struct {
			Ports []struct {
				PublicPort int    `json:"PublicPort"`
				Type       string `json:"Type"`
			} `json:"Ports"`
		}
		if json.Unmarshal(data, &containers) == nil {
			for _, c := range containers {
				for _, p := range c.Ports {
					if p.Type == "tcp" && p.PublicPort > 0 {
						if _, ok := ports[p.PublicPort]; !ok {
							ports[p.PublicPort] = p.PublicPort
						}
					}
				}
			}
		}
	}

	// Порты <1024 биндит setuid-хелпер boxlet-priv: демон передаёт ему
	// список и перезапускает при изменении (bind без root невозможен,
	// а хелпер сбрасывает права сразу после bind).
	if privhelper.Installed() {
		priv := map[int]int{}
		for h, g := range ports {
			if h < 1024 {
				priv[h] = g
				delete(ports, h)
			}
		}
		d.reconcilePriv(priv)
	}
	fwd.SetPorts(ports)
}

// reconcilePriv приводит набор портов, делегированных boxlet-priv,
// к нужному: при изменении списка хелпер перезапускается.
func (d *daemon) reconcilePriv(priv map[int]int) {
	specs := make([]string, 0, len(priv))
	for h, g := range priv {
		specs = append(specs, fmt.Sprintf("%d:%d", h, g))
	}
	sort.Strings(specs)
	key := strings.Join(specs, ",")

	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	if d.privCmd != nil && d.privCmd.Process != nil &&
		d.privCmd.Process.Signal(syscall.Signal(0)) == nil && d.privKey == key {
		return // актуальный хелпер уже работает
	}
	if d.privCmd != nil && d.privCmd.Process != nil {
		d.privCmd.Process.Signal(syscall.SIGTERM)
		d.privCmd.Wait()
		d.privCmd = nil
	}
	d.privKey = key
	if len(specs) == 0 {
		return
	}
	path, err := privhelper.Path()
	if err != nil {
		return
	}
	args := append([]string{"forward"}, specs...)
	cmd := exec.Command(path, args...)
	logFile, err := os.OpenFile(filepath.Join(d.layout.LogsDir, "boxlet-priv.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	if err := cmd.Start(); err != nil {
		log.Printf("boxletd: boxlet-priv: %v", err)
		return
	}
	log.Printf("boxletd: boxlet-priv (pid %d): порты %s", cmd.Process.Pid, key)
}

// startError возвращает ошибку последней попытки запуска, если была.
func (d *daemon) lastStartError() error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	return d.startErr
}

// postStart — действия после поднятия бэкенда:
// автосоздание сетей из конфига и symlink сокета.
func (d *daemon) postStart(ctx context.Context) {
	for _, network := range d.cfg.AutoNetworks {
		if out, err := d.docker(ctx, "network", "inspect", network); err == nil && len(out) > 0 {
			continue
		}
		if _, err := d.docker(ctx, "network", "create", network); err != nil {
			log.Printf("boxletd: создать сеть %s: %v", network, err)
		} else {
			log.Printf("boxletd: создана сеть %s", network)
		}
	}
	if d.cfg.LinkDockerSock {
		if err := linkDockerSock(d.layout.DockerSock); err != nil {
			log.Printf("boxletd: symlink /var/run/docker.sock: %v", err)
		}
	}
}

// docker выполняет операцию с dockerd через прокси-сокет.
// Поддержаны операции, нужные демону самому: network inspect/create.
func (d *daemon) docker(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) >= 3 && args[0] == "network" && args[1] == "inspect" {
		return d.dockerAPI(ctx, http.MethodGet, "/networks/"+args[2], nil)
	}
	if len(args) >= 3 && args[0] == "network" && args[1] == "create" {
		payload := fmt.Sprintf(`{"Name":%q}`, args[2])
		return d.dockerAPI(ctx, http.MethodPost, "/networks/create", strings.NewReader(payload))
	}
	return nil, fmt.Errorf("неподдерживаемая операция: %s", strings.Join(args, " "))
}

// dockerAPI выполняет простой запрос к Docker API через прокси-сокет.
func (d *daemon) dockerAPI(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://boxlet"+path, body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", d.layout.DockerSock)
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("не найдено")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("docker API %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, _ := d.be.Status(r.Context())
	var startErr string
	if err := d.lastStartError(); err != nil {
		startErr = err.Error()
	}
	out := map[string]any{
		"backend":   d.cfg.Backend,
		"running":   st.Running,
		"state":     st.State,
		"detail":    st.Detail,
		"version":   st.Version,
		"socket":    d.layout.DockerSock,
		"start_err": startErr,
	}
	if sh, ok := d.be.(backend.Sheller); ok {
		if addr, err := sh.AgentAddr(); err == nil {
			out["agent_addr"] = addr
		}
	}
	writeJSON(w, out)
}

func (d *daemon) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	// Асинхронно: загрузка VM может занимать минуты,
	// прогресс смотрим через /api/status.
	go d.startBackend(context.Background())
	writeJSON(w, map[string]string{"status": "starting"})
}

func (d *daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if err := d.stopEngine(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// stopEngine останавливает движок. Контекст — Background, а не контекст
// запроса: обрыв HTTP-соединения (трей шлёт POST и не ждёт ответа) не
// должен прерывать Stop посередине — иначе движок застревал в состоянии
// «stopping» навсегда, а старт после этого падал с «уже запущен».
// Вызовы сериализованы opMu (см. поле daemon).
func (d *daemon) stopEngine() error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.stopPortForwarding()
	d.px.Stop()
	return d.be.Stop(context.Background())
}

// handleContainers проксирует GET /containers/json в dockerd.
func (d *daemon) handleContainers(w http.ResponseWriter, r *http.Request) {
	data, err := d.dockerAPI(r.Context(), http.MethodGet, "/containers/json?all=1", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
