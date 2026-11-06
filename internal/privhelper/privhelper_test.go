//go:build unix

package privhelper

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runInstall выполняет installScript от имени текущего пользователя
// (владелец копии — он же, вместо root:wheel).
func runInstall(t *testing.T, src, dst, sum string) error {
	t.Helper()
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	out, err := exec.Command("sh", "-c", installScript(src, dst, sum, owner)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func writeBin(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestInstallScriptInstallsSetuidCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "boxlet-priv")
	os.MkdirAll(filepath.Dir(src), 0o755)
	sum := writeBin(t, src, "helper-v2")
	dst := filepath.Join(dir, "PrivilegedHelperTools", "boxlet-priv")

	if err := runInstall(t, src, dst, sum); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("права %v, ожидались setuid + 0755", fi.Mode())
	}
	if got, _ := os.ReadFile(dst); string(got) != "helper-v2" {
		t.Errorf("содержимое %q", got)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Error("временная копия осталась")
	}
}

// Раньше установка открывала назначение с O_TRUNC до чтения источника:
// если они совпадали (boxlet из ~/.boxlet/bin), хелпер обнулялся.
func TestInstallScriptSourceEqualsDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boxlet-priv")
	sum := writeBin(t, path, "helper-same-file")
	if err := runInstall(t, path, path, sum); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "helper-same-file" {
		t.Fatalf("после установки поверх себя содержимое %q", got)
	}
}

// Бинарь подменили после подсчёта хэша — setuid не ставится,
// установленный хелпер не трогается.
func TestInstallScriptRejectsSwappedBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "boxlet-priv-src")
	sum := writeBin(t, src, "trusted")
	dst := filepath.Join(dir, "boxlet-priv")
	writeBin(t, dst, "installed-v1")

	os.WriteFile(src, []byte("malicious"), 0o755)
	err := runInstall(t, src, dst, sum)
	if err == nil || !strings.Contains(err.Error(), "контрольная сумма") {
		t.Fatalf("ожидалась ошибка контрольной суммы, получено: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "installed-v1" {
		t.Errorf("установленный хелпер изменён: %q", got)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Error("непроверенная копия осталась")
	}
}

// Путь из окружения пользователя не должен стать командой root.
func TestInstallScriptQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	weird := filepath.Join(dir, `it's $(touch `+marker+`) "x"`)
	os.MkdirAll(weird, 0o755)
	src := filepath.Join(weird, "boxlet-priv")
	sum := writeBin(t, src, "helper")
	dst := filepath.Join(weird, "dst", "boxlet-priv")

	if err := runInstall(t, src, dst, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("подстановка команды из пути выполнилась")
	}
	if got, _ := os.ReadFile(dst); string(got) != "helper" {
		t.Errorf("содержимое %q", got)
	}
}

func TestAppleScriptQuote(t *testing.T) {
	got := appleScriptQuote(`echo "a\b"`)
	if want := `"echo \"a\\b\""`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// Результат должен разбираться osascript как строка и давать исходник.
	out, err := exec.Command("osascript", "-e", "return "+appleScriptQuote(`it's $(x) "q" \n`)).Output()
	if err != nil {
		t.Skip("osascript недоступен:", err)
	}
	if !bytes.Equal(bytes.TrimRight(out, "\n"), []byte(`it's $(x) "q" \n`)) {
		t.Errorf("osascript вернул %q", out)
	}
}

// Полная цепочка, как в Install, но без administrator privileges:
// AppleScript-литерал → do shell script → установка.
func TestInstallViaOsascript(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript недоступен")
	}
	dir := t.TempDir()
	weird := filepath.Join(dir, `a'b "c" $(d)`)
	os.MkdirAll(weird, 0o755)
	src := filepath.Join(weird, "boxlet-priv")
	sum := writeBin(t, src, "helper-osa")
	dst := filepath.Join(weird, "out", "boxlet-priv")
	owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	script := "do shell script " + appleScriptQuote(installScript(src, dst, sum, owner))
	if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("osascript: %v: %s", err, out)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("установка через osascript: %v, права %v", err, fi.Mode())
	}
}

func TestHasPrivilegedPorts(t *testing.T) {
	cases := map[string]bool{
		"80:80": true, "443:8443": true, "1023:1": true,
		"1024:80": false, "8080:80": false, "bad": false, "": false,
	}
	for spec, want := range cases {
		if got := hasPrivilegedPorts([]string{spec}); got != want {
			t.Errorf("%q: %v, ожидалось %v", spec, got, want)
		}
	}
	if !hasPrivilegedPorts([]string{"8080:80", "80:80"}) {
		t.Error("список с одним портом <1024 должен требовать хелпер")
	}
}

func TestStateNeedsInstall(t *testing.T) {
	for st, want := range map[State]bool{
		NotNeeded: false, UpToDate: false, Missing: true, Outdated: true, Legacy: true,
	} {
		if st.NeedsInstall() != want {
			t.Errorf("%v: NeedsInstall=%v", st, !want)
		}
	}
}
