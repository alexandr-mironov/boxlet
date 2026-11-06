package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// boxlet init вызывается и при обновлении (install.sh): существующий
// конфиг с пользовательскими настройками не должен перезаписываться.
func TestInitKeepsExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BOXLET_HOME", home)
	path := filepath.Join(home, "config.json")
	// darwin-vz: его Prepare без boxlet-vm рядом с тестовым бинарём сразу
	// возвращает ошибку и ничего не скачивает.
	orig := `{"backend":"darwin-vz","cpus":3,"mac":"02:00:00:00:00:01","forwarded_ports":["8443:443"],"api_port":9999}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != orig {
		t.Fatalf("конфиг перезаписан:\n%s", got)
	}
	for _, d := range []string{"images", "data", "logs", "run"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Errorf("каталог %s не создан: %v", d, err)
		}
	}
}

func TestInitCreatesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BOXLET_HOME", home)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil || cfg["backend"] == nil {
		t.Fatalf("конфиг не создан: %v %s", err, data)
	}
}
