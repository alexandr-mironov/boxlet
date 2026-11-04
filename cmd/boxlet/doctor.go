package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/config"
	"github.com/alexandr-mironov/boxlet/internal/paths"
	"github.com/alexandr-mironov/boxlet/internal/privhelper"
)

type check struct {
	name string
	ok   bool
	hint string
}

// cmdDoctor проверяет готовность окружения к работе с boxlet:
// сокет, симлинк, сети, авторизация в реестрах, платформа.
func cmdDoctor(args []string) error {
	cfg, _ := config.Load()
	root, err := config.Dir()
	if err != nil {
		return err
	}
	layout := paths.New(root)

	checks := []check{
		checkDockerCLI(),
		checkSocket(layout.DockerSock),
		checkVarRunSock(layout.DockerSock),
		checkNetwork(cfg, layout.DockerSock),
		checkRegistryAuth(),
		checkPlatform(cfg),
		checkMounts(cfg),
		checkPrivHelper(cfg),
	}

	failed := 0
	for _, c := range checks {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			failed++
		}
		fmt.Printf("%s %s\n", mark, c.name)
		if !c.ok && c.hint != "" {
			fmt.Printf("    → %s\n", c.hint)
		}
	}
	fmt.Println()
	if failed == 0 {
		fmt.Println("окружение готово к работе")
	} else {
		fmt.Printf("проблем: %d — исправьте подсказки выше и повторите boxlet doctor\n", failed)
	}
	return nil
}

func checkDockerCLI() check {
	if _, err := os.Stat("/usr/local/bin/docker"); err != nil {
		if _, err := os.Lstat("/opt/homebrew/bin/docker"); err != nil {
			return check{"docker CLI в PATH", false, "установите docker cli: brew install docker"}
		}
	}
	return check{"docker CLI в PATH", true, ""}
}

// dockerPing выполняет GET /_ping через сокет и возвращает ошибку при недоступности.
func dockerPing(socket string) error {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /_ping HTTP/1.1\r\nHost: boxlet\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("пустой ответ")
	}
	return nil
}

func checkSocket(socket string) check {
	if err := dockerPing(socket); err != nil {
		return check{"docker API на " + socket, false,
			"движок не запущен: boxlet start"}
	}
	return check{"docker API на " + socket, true, ""}
}

func checkVarRunSock(socket string) check {
	if runtime.GOOS == "windows" {
		return check{"/var/run/docker.sock", true, ""}
	}
	target, err := os.Readlink("/var/run/docker.sock")
	if err != nil {
		return check{"/var/run/docker.sock → сокет boxlet", false,
			"установите хелпер: boxlet priv install — или вручную: sudo ln -s " + socket + " /var/run/docker.sock"}
	}
	if target != socket {
		return check{"/var/run/docker.sock → сокет boxlet", false,
			"симлинк указывает на " + target + " (возможно, запущен OrbStack/Docker Desktop)"}
	}
	return check{"/var/run/docker.sock → сокет boxlet", true, ""}
}

// checkNetwork проверяет наличие сетей из cfg.AutoNetworks.
func checkNetwork(cfg *config.Config, socket string) check {
	if len(cfg.AutoNetworks) == 0 {
		return check{"автосоздание сетей не настроено (auto_networks)", true, ""}
	}
	if err := dockerPing(socket); err != nil {
		return check{"автосети " + fmt.Sprint(cfg.AutoNetworks), false, "сначала запустите движок: boxlet start"}
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socket)
		},
	}}
	for _, network := range cfg.AutoNetworks {
		resp, err := client.Get("http://boxlet/networks/" + network)
		if err != nil {
			return check{"сеть " + network, false, err.Error()}
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return check{"сеть " + network, false,
				"создайте: docker network create " + network + " (или перезапустите boxlet start)"}
		}
	}
	return check{"сети " + fmt.Sprint(cfg.AutoNetworks), true, ""}
}

// checkRegistryAuth проверяет, что ~/.docker/config.json существует
// и читается: boxlet не подменяет DOCKER_CONFIG, поэтому авторизация
// в реестрах работает так же, как с обычным Docker.
func checkRegistryAuth() check {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil {
		return check{"конфиг реестров (~/.docker/config.json)", false,
			"файл отсутствует; выполните docker login <registry> при необходимости"}
	}
	var cfg struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return check{"конфиг реестров (~/.docker/config.json)", false,
			"не читается: " + err.Error()}
	}
	return check{"конфиг реестров (~/.docker/config.json)", true, ""}
}

// checkPlatform — специфика Apple Silicon: Rosetta для linux/amd64.
func checkPlatform(cfg *config.Config) check {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if !cfg.Rosetta {
			return check{"эмуляция linux/amd64 (Rosetta)", false,
				"включите rosetta в ~/.boxlet/config.json — часть образов платформы собрана под amd64"}
		}
		return check{"эмуляция linux/amd64 (Rosetta)", true, ""}
	}
	return check{"платформа " + runtime.GOOS + "/" + runtime.GOARCH, true, ""}
}

// checkMounts проверяет, что каталоги проектов расшарены в VM.
func checkMounts(cfg *config.Config) check {
	for _, m := range cfg.Mounts {
		if _, err := os.Stat(m); err != nil {
			return check{"каталог " + m + " существует", false,
				"поправьте mounts в ~/.boxlet/config.json"}
		}
	}
	return check{"каталоги проектов расшарены (" + fmt.Sprint(cfg.Mounts) + ")", true, ""}
}

// checkPrivHelper проверяет setuid-хелпер, если в forwarded_ports
// есть порты <1024 (их проброс без хелпера невозможен).
func checkPrivHelper(cfg *config.Config) check {
	if runtime.GOOS != "darwin" {
		return check{"privileged helper", true, ""}
	}
	need := false
	for _, spec := range cfg.ForwardedPorts {
		host, _, _ := strings.Cut(spec, ":")
		if p, err := strconv.Atoi(host); err == nil && p < 1024 {
			need = true
			break
		}
	}
	if !need {
		return check{"privileged helper (порты <1024 не запрошены)", true, ""}
	}
	if !privhelper.Installed() {
		return check{"privileged helper (порты <1024)", false,
			"установите: boxlet priv install (один раз, с паролем администратора)"}
	}
	return check{"privileged helper (порты <1024)", true, ""}
}
