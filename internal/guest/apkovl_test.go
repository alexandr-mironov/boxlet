package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

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
	data, err := BuildApkovl("https://mirror.example/alpine", "3.21",
		[]string{"/Users/test/projects"}, true, []byte("AGENTBIN"), "tok123")
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
}

func TestBuildApkovlMinimal(t *testing.T) {
	data, err := BuildApkovl("https://mirror.example/alpine", "latest-stable",
		nil, false, nil, "")
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
