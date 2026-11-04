package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/alexandr-mironov/boxlet/internal/certs"
)

func testBundle(t *testing.T) *certs.Bundle {
	t.Helper()
	b, err := certs.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// untar распаковывает apkovl в map имя → (тело, режим).
func untar(t *testing.T, data []byte) map[string]tarFile {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	out := map[string]tarFile{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		if hdr.Typeflag == tar.TypeReg {
			body, err = io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
		}
		out[hdr.Name] = tarFile{name: hdr.Name, body: body, mode: hdr.Mode, linkname: hdr.Linkname}
	}
	return out
}

func TestBuildApkovl(t *testing.T) {
	bundle := testBundle(t)
	data, err := BuildApkovl("https://mirror.example/alpine", "3.21",
		[]string{"/Users/test/projects"}, true, []byte("AGENTBIN"), "tok123", bundle)
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, data)

	if !strings.Contains(string(files["etc/network/interfaces"].body), "iface eth0 inet dhcp") {
		t.Error("interfaces: нет dhcp на eth0")
	}
	if !strings.Contains(string(files["etc/apk/repositories"].body), "https://mirror.example/alpine/3.21/main") {
		t.Error("repositories: нет зеркала/релиза")
	}
	boot := string(files["etc/local.d/boxlet.start"].body)
	for _, want := range []string{"dockerd", "mount -t virtiofs share0", "Rosetta"} {
		if !strings.Contains(boot, want) {
			t.Errorf("bootstrap: нет %q", want)
		}
	}
	if !bytes.Equal(files["usr/local/bin/boxlet-agent"].body, []byte("AGENTBIN")) {
		t.Error("бинарь агента не вошёл в apkovl")
	}
	tok := files["etc/boxlet-agent.token"]
	if string(tok.body) != "tok123" || tok.mode != 0o600 {
		t.Errorf("токен: body=%q mode=%o (ожидал 600)", tok.body, tok.mode)
	}

	key := files["etc/docker/tls/server-key.pem"]
	if !bytes.Equal(key.body, bundle.ServerKey) || key.mode != 0o600 {
		t.Errorf("ключ dockerd: совпадает=%v mode=%o (ожидал 600)", bytes.Equal(key.body, bundle.ServerKey), key.mode)
	}
	if _, ok := files["etc/docker/tls/client-key.pem"]; ok {
		t.Error("клиентский ключ не должен попадать в гостевую ОС")
	}
	if strings.Contains(boot, "@DOCKER_PORT@") || strings.Contains(boot, "%!") {
		t.Error("bootstrap: не подставлен порт или ошибка форматирования")
	}
	for _, want := range []string{`"tlsverify":true`, "tcp://0.0.0.0:" + strconv.Itoa(DockerPort), "busybox.static"} {
		if !strings.Contains(boot, want) {
			t.Errorf("bootstrap: нет %q", want)
		}
	}
	if !strings.Contains(string(files["etc/local.d/boxlet.stop"].body), "docker.pid") {
		t.Error("нет stop-скрипта dockerd")
	}
}

func TestBuildApkovlMinimal(t *testing.T) {
	data, err := BuildApkovl("https://mirror.example/alpine", "latest-stable",
		nil, false, nil, "", testBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, data)
	boot := string(files["etc/local.d/boxlet.start"].body)
	if strings.Contains(boot, "virtiofs") || strings.Contains(boot, "Rosetta") {
		t.Error("без mounts/rosetta в bootstrap не должно быть virtiofs и Rosetta")
	}
	if _, ok := files["usr/local/bin/boxlet-agent"]; ok {
		t.Error("без бинаря агента его не должно быть в apkovl")
	}
}

func TestAgentToken(t *testing.T) {
	dir := t.TempDir()
	t1, err := AgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != 64 {
		t.Errorf("токен должен быть 64 hex-символа, получил %d", len(t1))
	}
	t2, err := AgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 {
		t.Error("токен должен сохраняться между вызовами")
	}
}
