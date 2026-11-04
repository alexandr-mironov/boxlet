package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/guest"
)

// cmdLogs показывает логи: boxlet logs [daemon|vm|helper|dockerd|agent] [-f].
// daemon/vm/helper — локальные файлы; dockerd/agent — из гостевой ОС
// через boxlet-agent.
func cmdLogs(args []string) error {
	target := "daemon"
	follow := false
	for _, a := range args {
		switch a {
		case "-f", "--follow":
			follow = true
		default:
			target = a
		}
	}

	root, err := config.Dir()
	if err != nil {
		return err
	}
	cfg, _ := config.Load()
	backend := ""
	if cfg != nil {
		backend = cfg.Backend
	}

	switch target {
	case "daemon":
		return tailLocal(filepath.Join(root, "logs", "boxletd.log"), follow)
	case "vm", "serial":
		name := "vm-serial.log"
		if backend == "qemu" {
			name = "qemu-serial.log"
		}
		return tailLocal(filepath.Join(root, "logs", name), follow)
	case "helper":
		name := "boxlet-vm.log"
		if backend == "qemu" {
			name = "qemu.log"
		}
		return tailLocal(filepath.Join(root, "logs", name), follow)
	case "dockerd":
		return tailGuest("/var/log/dockerd.log", follow)
	case "agent":
		return tailGuest("/var/log/boxlet-agent.log", follow)
	default:
		return fmt.Errorf("неизвестный лог: %s (доступны: daemon, vm, helper, dockerd, agent)", target)
	}
}

// tailLocal выводит последние ~100 строк локального файла и,
// с follow=true, дочитывает новые (пауза 500 мс).
func tailLocal(path string, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	const keep = 100
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	offset := int64(len(data))
	os.Stdout.WriteString(strings.Join(lines, "\n"))

	if !follow {
		return nil
	}
	for {
		time.Sleep(500 * time.Millisecond)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() < offset {
			offset = 0 // лог ротировался/пересоздан
		}
		if fi.Size() == offset {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		n, err := io.Copy(os.Stdout, f)
		if err != nil {
			return err
		}
		offset += n
	}
}

// tailGuest читает лог из гостевой ОС через boxlet-agent.
func tailGuest(path string, follow bool) error {
	addr, token, err := agentCredentials()
	if err != nil {
		return err
	}
	argv := []string{"tail", "-100", path}
	if follow {
		argv = []string{"tail", "-100", "-f", path}
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("агент гостевой ОС недоступен: %w", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n%s\n", token, strings.Join(argv, "\x00"))
	// tail не читает stdin, но агент ждёт его EOF перед завершением
	// команды (exec.Cmd копирует stdin до конца) — закрываем запись.
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	_, err = io.Copy(os.Stdout, conn)
	return err
}

// agentCredentials возвращает адрес гостевого агента и токен.
func agentCredentials() (addr, token string, err error) {
	base, err := daemonURL()
	if err != nil {
		return "", "", err
	}
	resp, err := http.Get(base + "/api/status")
	if err != nil {
		return "", "", fmt.Errorf("демон недоступен: %w (boxlet start)", err)
	}
	var st struct {
		AgentAddr string `json:"agent_addr"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.AgentAddr == "" {
		return "", "", fmt.Errorf("движок не запущен: boxlet start")
	}
	root, err := config.Dir()
	if err != nil {
		return "", "", err
	}
	token, err = guest.AgentToken(root)
	if err != nil {
		return "", "", err
	}
	return st.AgentAddr, token, nil
}
