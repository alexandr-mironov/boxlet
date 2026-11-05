package guest

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/certs"
)

// Секреты гостевой ОС — TLS-пара dockerd и токен агента — едут в VM
// не в apkovl: URL оверлея передаётся в kernel cmdline, а её видно
// в серийной консоли (логи VM) и в аргументах процесса VM. Вместо этого
// секреты лежат в файле, доступном только владельцу ~/.boxlet,
// и попадают в гостя по каналу, к которому нет доступа с хоста:
//   - qemu: fw_cfg (-fw_cfg name=SecretsFwCfgName,file=...), в госте —
//     /sys/firmware/qemu_fw_cfg/by_name/<имя>/raw, читает только root;
//   - darwin-vz: virtiofs-шара SecretsShareTag каталога с этим файлом.
const (
	SecretsFwCfgName = "opt/boxlet/secrets"
	SecretsShareTag  = "boxlet-secrets"
	SecretsFileName  = "secrets.tar"
)

// SecretsDir — каталог с файлом секретов (0700): <root>/run/guest-secrets.
// Отдельный каталог нужен darwin-vz: virtiofs шарит каталог целиком.
func SecretsDir(root string) string {
	return filepath.Join(root, "run", "guest-secrets")
}

// WriteSecrets собирает tar с секретами гостя и пишет его в SecretsDir
// (файл 0600). Возвращает путь к файлу.
func WriteSecrets(root, token string, tls *certs.Bundle) (string, error) {
	data, err := BuildSecrets(token, tls)
	if err != nil {
		return "", err
	}
	dir := SecretsDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, SecretsFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// BuildSecrets собирает несжатый tar (busybox tar в госте распаковывает
// его без gzip) с путями относительно корня гостевой ОС.
func BuildSecrets(token string, tls *certs.Bundle) ([]byte, error) {
	files := []tarFile{
		{name: "etc/docker/", mode: 0o755},
		{name: "etc/docker/tls/", mode: 0o700},
		{name: "etc/docker/tls/ca.pem", body: tls.CA, mode: 0o644},
		{name: "etc/docker/tls/server.pem", body: tls.ServerCert, mode: 0o644},
		{name: "etc/docker/tls/server-key.pem", body: tls.ServerKey, mode: 0o600},
		// Токен агента: только root гостевой ОС.
		{name: "etc/boxlet-agent.token", body: []byte(token), mode: 0o600},
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTar(tw, files); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeTar пишет элементы оверлея в tar.
func writeTar(tw *tar.Writer, files []tarFile) error {
	for _, f := range files {
		hdr := &tar.Header{
			Name:    f.name,
			Mode:    f.mode,
			ModTime: time.Now(),
		}
		switch {
		case f.linkname != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = f.linkname
		case len(f.body) == 0 && f.name[len(f.name)-1] == '/':
			hdr.Typeflag = tar.TypeDir
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(f.body); err != nil {
				return err
			}
		}
	}
	return nil
}
