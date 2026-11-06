// Package privhelper — установка и проверка привилегированного
// setuid-хелпера boxlet-priv (см. cmd/boxlet-priv).
package privhelper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alexandr-mironov/boxlet/internal/config"
)

// Dir — каталог установленного хелпера. Он принадлежит root: файл
// с setuid-битом нельзя держать там, где пользователь (или любой его
// процесс) может его удалить и подложить свой, — например, в ~/.boxlet/bin,
// куда install.sh копирует бинари релиза.
const Dir = "/Library/PrivilegedHelperTools"

// Path — путь установленного хелпера. Отдельная копия, а не бинарь из
// каталога сборки или релиза: setuid-бит не должен слетать при пересборке
// или обновлении boxlet.
func Path() (string, error) {
	return filepath.Join(Dir, "boxlet-priv"), nil
}

// legacyPath — где хелпер лежал раньше (в каталоге пользователя).
func legacyPath() (string, error) {
	root, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "bin", "boxlet-priv"), nil
}

// Install устанавливает srcBin как root:wheel 4755 в Path. Повышение
// прав — через osascript с GUI-запросом пароля администратора (как
// у Docker Desktop) или, если sudo=true, через sudo в терминале: так
// установщик спрашивает пароль один раз за всю установку.
//
// Файл копирует уже root, в каталог root, и setuid ставится только
// после сверки хэша копии с хэшем, посчитанным здесь: подменить бинарь
// между проверкой и установкой нельзя. Источник при этом может совпадать
// с любым путём — копия не затирает его.
func Install(srcBin string, sudo bool) error {
	sum, err := fileSHA256(srcBin)
	if err != nil {
		return fmt.Errorf("бинарь хелпера не найден: %s (соберите: make build): %w", srcBin, err)
	}
	dst, err := Path()
	if err != nil {
		return err
	}
	script := installScript(srcBin, dst, sum, "root:wheel")
	if sudo {
		// sudo сам спросит пароль в терминале (или возьмёт уже выданный).
		cmd := exec.Command("sudo", "sh", "-c", script)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("установка через sudo не удалась: %w", err)
		}
	} else {
		cmd := exec.Command("osascript", "-e",
			"do shell script "+appleScriptQuote(script)+" with administrator privileges")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("установка с правами root отклонена или не удалась: %v (%s)",
				err, strings.TrimSpace(string(out)))
		}
	}
	if !Installed() {
		return fmt.Errorf("хелпер установлен в %s, но владелец или setuid-бит не применились", dst)
	}
	removeLegacy()
	return nil
}

// State — состояние хелпера относительно бинаря srcBin из текущей сборки.
type State int

const (
	NotNeeded State = iota // портов <1024 нет, хелпер никогда не ставился
	UpToDate               // установлен ровно srcBin
	Missing                // нужен (порты <1024 в конфиге), но не установлен
	Outdated               // установлен, но отличается от srcBin
	Legacy                 // установлен только в старом месте (~/.boxlet/bin)
)

func (s State) String() string {
	return [...]string{
		NotNeeded: "не нужен (портов <1024 в forwarded_ports нет)",
		UpToDate:  "установлен, актуален",
		Missing:   "не установлен, а в forwarded_ports есть порты <1024",
		Outdated:  "установлен, но устарел",
		Legacy:    "установлен в старое место (~/.boxlet/bin) и не используется",
	}[s]
}

// NeedsInstall — нужно ли (пере)устанавливать хелпер.
func (s State) NeedsInstall() bool { return s >= Missing }

// Status сравнивает установленный хелпер с srcBin и с конфигом:
// хелпер нужен, если он уже стоял (сейчас или в старом месте) или если
// в forwarded_ports есть порты <1024.
func Status(srcBin string, forwardedPorts []string) State {
	if Installed() {
		dst, _ := Path()
		want, err1 := fileSHA256(srcBin)
		have, err2 := fileSHA256(dst)
		if err1 != nil || err2 != nil || want != have {
			return Outdated
		}
		return UpToDate
	}
	if LegacyInstalled() {
		return Legacy
	}
	if hasPrivilegedPorts(forwardedPorts) {
		return Missing
	}
	return NotNeeded
}

// hasPrivilegedPorts — есть ли в спеках "host:guest" хостовый порт <1024.
func hasPrivilegedPorts(specs []string) bool {
	for _, spec := range specs {
		host, _, _ := strings.Cut(spec, ":")
		if p, err := strconv.Atoi(host); err == nil && p > 0 && p < 1024 {
			return true
		}
	}
	return false
}

// installScript — shell-команда установки, которую выполняет root.
// owner — владелец копии (в тестах — текущий пользователь).
// Копия кладётся рядом под временным именем и подменяет установленную
// атомарным mv: работающий хелпер продолжает работать со старым inode.
func installScript(src, dst, sum, owner string) string {
	tmp := dst + ".new"
	q := shellQuote
	return strings.Join([]string{
		"set -e",
		"mkdir -p " + q(filepath.Dir(dst)),
		"rm -f " + q(tmp),
		"cp " + q(src) + " " + q(tmp),
		"chown " + q(owner) + " " + q(tmp),
		"chmod 0755 " + q(tmp),
		// Сверяем то, что реально скопировано, а не исходник.
		`if [ "$(shasum -a 256 < ` + q(tmp) + ` | cut -d' ' -f1)" != ` + q(sum) + ` ]; then ` +
			`rm -f ` + q(tmp) + `; echo "boxlet-priv: контрольная сумма копии не совпала" >&2; exit 1; fi`,
		"chmod 4755 " + q(tmp),
		"mv -f " + q(tmp) + " " + q(dst),
	}, "; ")
}

// LegacyInstalled — хелпер установлен по-старому, в ~/.boxlet/bin,
// а в Path его нет: демон такой хелпер не использует, нужна
// переустановка (boxlet priv install).
func LegacyInstalled() bool {
	path, err := legacyPath()
	return err == nil && isSetuidRoot(path) && !Installed()
}

// removeLegacy удаляет setuid-копию хелпера из старого места
// (~/.boxlet/bin): она больше не используется, но оставалась бы
// исполняемой с правами root. Каталог принадлежит пользователю,
// поэтому удалить root-файл из него можно без пароля.
func removeLegacy() {
	path, err := legacyPath()
	if err != nil {
		return
	}
	if isSetuidRoot(path) {
		os.Remove(path)
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shellQuote — строка в одинарных кавычках sh: внутри ничего
// не раскрывается ($, `, \), поэтому путь не может стать командой.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptQuote — строковый литерал AppleScript: экранируются
// только обратный слэш и двойная кавычка.
func appleScriptQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
