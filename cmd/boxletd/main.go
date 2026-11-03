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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/darwinvz"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/linux"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/qemu"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/proxy"
	"github.com/alexandr-mironov/boxlet/internal/webui"
)

type daemon struct {
	cfg    *config.Config
	layout paths.Layout
	be     backend.Backend
	px     *proxy.Proxy

	startMu     sync.Mutex
	starting    bool
	startErr    error
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

	d := &daemon{cfg: cfg, layout: layout, be: be, px: proxy.New(be, layout.DockerSock)}

	// Control API + веб-интерфейс — поднимаем СРАЗУ, до запуска бэкенда:
	// загрузка VM занимает минуты, и CLI должен видеть прогресс.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/start", d.handleStart)
	mux.HandleFunc("/api/stop", d.handleStop)
	mux.HandleFunc("/api/containers", d.handleContainers)
	mux.Handle("/", webui.Handler())

	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.APIPort),
		Handler: mux,
	}
	go func() {
		log.Printf("boxletd: веб-интерфейс на http://%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	log.Printf("boxletd: движок готов")
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
	writeJSON(w, map[string]any{
		"backend":   d.cfg.Backend,
		"running":   st.Running,
		"state":     st.State,
		"detail":    st.Detail,
		"version":   st.Version,
		"socket":    d.layout.DockerSock,
		"start_err": startErr,
	})
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
	d.px.Stop()
	if err := d.be.Stop(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
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
