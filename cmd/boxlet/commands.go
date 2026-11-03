package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/backend"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/darwinvz"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/linux"
	_ "github.com/alexandr-mironov/boxlet/internal/backend/qemu"
	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
)

// cmdInit — первичная настройка инстанса.
func cmdInit(args []string) error {
	cfg := config.Default()
	root, err := config.Dir()
	if err != nil {
		return err
	}
	layout := paths.New(root)
	for _, d := range []string{root, layout.ImagesDir, layout.DataDir, layout.LogsDir, filepath.Dir(layout.DockerSock)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("конфигурация: %s (backend=%s, cpu=%d, mem=%d MiB)\n",
		filepath.Join(root, "config.json"), cfg.Backend, cfg.CPUs, cfg.MemoryMiB)

	be, err := backend.New(cfg.Backend)
	if err != nil {
		return err
	}
	if err := be.Prepare(context.Background()); err != nil {
		fmt.Printf("подготовка бэкенда %s отложена: %v\n", cfg.Backend, err)
	}
	fmt.Println("init завершён. Дальше: boxlet start, затем boxlet doctor")
	return nil
}

// daemonURL возвращает адрес control API демона.
func daemonURL() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.APIPort), nil
}

// ensureDaemon поднимает boxletd, если он ещё не запущен.
func ensureDaemon() error {
	base, err := daemonURL()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get(base + "/api/status"); err == nil {
		return nil // уже работает
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	boxletd := filepath.Join(filepath.Dir(exe), "boxletd")
	if _, err := os.Stat(boxletd); err != nil {
		return fmt.Errorf("демон boxletd не найден рядом с CLI (%s); выполните make build", boxletd)
	}

	root, _ := config.Dir()
	logFile, err := os.OpenFile(filepath.Join(root, "logs", "boxletd.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(boxletd)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = daemonizeAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запустить boxletd: %w", err)
	}

	// Ждём, пока API поднимется.
	for range 20 {
		time.Sleep(250 * time.Millisecond)
		if _, err := client.Get(base + "/api/status"); err == nil {
			return nil
		}
	}
	return fmt.Errorf("boxletd не ответил за 5 секунд, см. лог %s", logFile.Name())
}

func apiPost(path string) error {
	base, err := daemonURL()
	if err != nil {
		return err
	}
	resp, err := http.Post(base+path, "application/json", nil)
	if err != nil {
		return fmt.Errorf("демон недоступен: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s", string(body))
	}
	return nil
}

func cmdStart(args []string) error {
	if err := ensureDaemon(); err != nil {
		return err
	}
	if err := apiPost("/api/start"); err != nil {
		return err
	}
	fmt.Println("движок запускается (первая загрузка VM — несколько минут)…")

	// Опрашиваем статус до готовности или ошибки.
	base, err := daemonURL()
	if err != nil {
		return err
	}
	lastDetail := ""
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		resp, err := http.Get(base + "/api/status")
		if err != nil {
			continue
		}
		var st struct {
			State    string `json:"state"`
			Detail   string `json:"detail"`
			Running  bool   `json:"running"`
			StartErr string `json:"start_err"`
		}
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()

		if st.Detail != lastDetail && st.Detail != "" {
			fmt.Printf("  %s\n", st.Detail)
			lastDetail = st.Detail
		}
		if st.Running {
			fmt.Println("движок запущен; docker API: ~/.boxlet/run/docker.sock")
			return nil
		}
		if st.StartErr != "" {
			return fmt.Errorf("запуск не удался: %s", st.StartErr)
		}
	}
	return fmt.Errorf("таймаут ожидания запуска; проверьте boxlet status и логи в ~/.boxlet/logs")
}

func cmdStop(args []string) error {
	if err := apiPost("/api/stop"); err != nil {
		return err
	}
	fmt.Println("движок остановлен")
	return nil
}

func cmdStatus(args []string) error {
	base, err := daemonURL()
	if err != nil {
		return err
	}
	resp, err := http.Get(base + "/api/status")
	if err != nil {
		fmt.Println("демон не запущен (boxlet start)")
		return nil
	}
	defer resp.Body.Close()
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	fmt.Printf("бэкенд:  %v\nсостояние: %v (%v)\nсокет:   %v\n",
		st["backend"], st["state"], st["detail"], st["socket"])
	return nil
}

// cmdShell открывает shell внутри VM бэкенда.
func cmdShell(args []string) error {
	return fmt.Errorf("появится вместе с первым работающим бэкендом (M1)")
}

// cmdDockerPassthrough пробрасывает команду в docker CLI хоста,
// предварительно указывая ему наш сокет.
func cmdDockerPassthrough(args []string) error {
	root, err := config.Dir()
	if err != nil {
		return err
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker CLI не найден в PATH")
	}
	cmd := exec.Command(docker, args...)
	cmd.Env = append(os.Environ(),
		"DOCKER_HOST=unix://"+paths.New(root).DockerSock)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// cmdUI открывает веб-интерфейс в браузере.
func cmdUI(args []string) error {
	base, err := daemonURL()
	if err != nil {
		return err
	}
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "windows":
		opener = "rundll32"
	default:
		opener = "xdg-open"
	}
	if runtime.GOOS == "windows" {
		return exec.Command(opener, "url.dll,FileProtocolHandler", base).Run()
	}
	return exec.Command(opener, base).Run()
}
