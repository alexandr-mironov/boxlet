package main

// Дополнительные хендлеры control API: live-метрики гостевой ОС,
// рестарт движка и просмотр логов (используются веб-интерфейсом).

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	"github.com/alexandr-mironov/boxlet/internal/guest"
)

// handleMetrics — GET /api/metrics: load average и память гостевой ОС
// через гостевой агент. 503, если движок остановлен или агент недоступен.
func (d *daemon) handleMetrics(w http.ResponseWriter, r *http.Request) {
	sh, ok := d.be.(backend.Sheller)
	if !ok {
		http.Error(w, "бэкенд без гостевого агента", http.StatusServiceUnavailable)
		return
	}
	addr, err := sh.AgentAddr()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	token, err := guest.AgentToken(d.root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out, err := backend.AgentExec(ctx, addr, token,
		[]string{"sh", "-c", "cat /proc/loadavg; grep -E 'MemTotal|MemAvailable' /proc/meminfo"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	var load string
	var memTotal, memAvail int64
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.Count(fields[0], ".") == 1 && load == "" {
			load = strings.Join(fields[:3], " ")
		}
		if len(fields) >= 2 {
			switch fields[0] {
			case "MemTotal:":
				fmt.Sscan(fields[1], &memTotal)
			case "MemAvailable:":
				fmt.Sscan(fields[1], &memAvail)
			}
		}
	}
	writeJSON(w, map[string]any{
		"load":          load,
		"mem_total_mib": memTotal / 1024,
		"mem_used_mib":  (memTotal - memAvail) / 1024,
	})
}

// handleRestart — POST /api/restart: останавливает бэкенд и поднимает
// заново. Асинхронно: загрузка VM занимает время, прогресс — /api/status.
func (d *daemon) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	go func() {
		log.Printf("boxletd: рестарт движка по запросу")
		d.stopPortForwarding()
		d.px.Stop()
		if err := d.be.Stop(context.Background()); err != nil {
			log.Printf("boxletd: рестарт: остановка: %v", err)
		}
		// Страховка: ждём, пока бэкенд реально отчитается об остановке,
		// чтобы Start не упал с «уже запущен» на границе состояний.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			st, _ := d.be.Status(context.Background())
			if !st.Running && st.State != "stopping" {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		d.startBackend(context.Background())
	}()
	writeJSON(w, map[string]string{"status": "restarting"})
}

// logFileFor возвращает путь локального лог-файла по имени цели.
// Имена совпадают с `boxlet logs` (плюс priv — лог setuid-хелпера).
func (d *daemon) logFileFor(name string) (string, bool) {
	qemu := d.cfg.Backend == "qemu"
	switch name {
	case "daemon":
		return filepath.Join(d.layout.LogsDir, "boxletd.log"), true
	case "vm":
		if qemu {
			return filepath.Join(d.layout.LogsDir, "qemu-serial.log"), true
		}
		return filepath.Join(d.layout.LogsDir, "vm-serial.log"), true
	case "helper":
		if qemu {
			return filepath.Join(d.layout.LogsDir, "qemu.log"), true
		}
		return filepath.Join(d.layout.LogsDir, "boxlet-vm.log"), true
	case "priv":
		return filepath.Join(d.layout.LogsDir, "boxlet-priv.log"), true
	}
	return "", false
}

// guestLogFor возвращает путь лога внутри гостевой ОС.
func guestLogFor(name string) (string, bool) {
	switch name {
	case "dockerd":
		return "/var/log/dockerd.log", true
	case "agent":
		return "/var/log/boxlet-agent.log", true
	}
	return "", false
}

// handleLogs — GET /api/logs?name=daemon&tail=200: последние N строк лога
// (локального файла или из гостевой ОС через агента). text/plain.
func (d *daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "daemon"
	}
	tail := 200
	if s := r.URL.Query().Get("tail"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			tail = min(n, 1000)
		}
	}

	var data []byte
	if path, ok := d.logFileFor(name); ok {
		raw, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, fmt.Sprintf("%s: %v", path, err), http.StatusNotFound)
			return
		}
		data = raw
	} else if path, ok := guestLogFor(name); ok {
		out, err := d.guestTail(r.Context(), path, tail)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(out)
		return
	} else {
		http.Error(w, "неизвестный лог: "+name+" (daemon, vm, helper, priv, dockerd, agent)", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(strings.Join(lines, "\n")))
}

// guestTail читает хвост лога из гостевой ОС через boxlet-agent.
func (d *daemon) guestTail(ctx context.Context, path string, n int) ([]byte, error) {
	sh, ok := d.be.(backend.Sheller)
	if !ok {
		return nil, fmt.Errorf("бэкенд без гостевого агента")
	}
	addr, err := sh.AgentAddr()
	if err != nil {
		return nil, err
	}
	token, err := guest.AgentToken(d.root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return backend.AgentExec(ctx, addr, token, []string{"tail", "-n", strconv.Itoa(n), path})
}
