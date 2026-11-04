package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexandr-mironov/boxlet/internal/certs"
)

// bootstrapScript — скрипт первой загрузки гостевой ОС.
// Запускается сервисом local (OpenRC) из /etc/local.d/boxlet.start:
// ставит docker из репозиториев Alpine, форматирует и монтирует
// data-диск под /var/lib/docker, настраивает и запускает dockerd.
// %s — секция virtiofs-шар и Rosetta (зависит от бэкенда).
const bootstrapScript = `#!/bin/sh
# boxlet: автонастройка гостевой ОС (генерируется, не редактировать)
# Лог в консоль (серийный лог VM).
exec >/dev/console 2>&1
set -x
echo "boxlet bootstrap: start"

# Подстраховка сети: если интерфейс без адреса — dhcp вручную.
if ! ip -4 addr show dev eth0 | grep -q "inet "; then
    ip link set eth0 up
    udhcpc -i eth0 -q
fi

apk update
# busybox-static — для служебного образа boxlet-agent: обычный
# /bin/busybox слинкован с musl и в пустом образе не запустится.
apk add docker e2fsprogs busybox-static

# Модули ядра: modloop-virt подключён как отдельный read-only
# virtio-диск (squashfs). Находим его по типу ФС и монтируем.
MODLOOP_DEV=$(blkid 2>/dev/null | grep squashfs | cut -d: -f1 | head -1)
if [ -n "$MODLOOP_DEV" ]; then
    mkdir -p /.modloop /lib/modules
    mount -o ro -t squashfs "$MODLOOP_DEV" /.modloop
    ln -sfn "/.modloop/modules/$(uname -r)" "/lib/modules/$(uname -r)"
fi
modprobe overlay || true
# ext4 нужен для data-диска; на чистой системе модуль ещё не загружен.
modprobe ext4 || true
%s
# Data-диск под данные dockerd: первый virtio-диск, который не modloop.
DATA_DEV=""
for d in /dev/vd?; do
    [ -b "$d" ] || continue
    blkid "$d" 2>/dev/null | grep -q squashfs && continue
    DATA_DEV=$d
    break
done
if [ -n "$DATA_DEV" ]; then
    # busybox blkid на пустом диске возвращает код 0 без вывода,
    # поэтому проверяем наличие сигнатуры по выводу, а не по коду.
    if ! blkid "$DATA_DEV" 2>/dev/null | grep -q "TYPE="; then
        mkfs.ext4 -q "$DATA_DEV"
    fi
    mkdir -p /var/lib/docker
    mountpoint -q /var/lib/docker || mount "$DATA_DEV" /var/lib/docker
fi

# Минимум, который нужен dockerd: в diskless netboot-режиме часть
# sysinit-сервисов OpenRC (sysfs, cgroups) не поднимается, поэтому
# монтируем сами.
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mkdir -p /sys/fs/cgroup
mountpoint -q /sys/fs/cgroup || mount -t cgroup2 none /sys/fs/cgroup || true

# dockerd слушает unix-сокет и TCP с обязательным клиентским
# TLS-сертификатом (tlsverify). TCP доступен с хоста (hostfwd qemu
# на 127.0.0.1 или IP гостя в vmnet у darwin-vz), и без сертификата
# туда мог бы постучаться любой локальный процесс, другая VM в vmnet
# или веб-страница через DNS rebinding. Сертификаты приходят в apkovl.
mkdir -p /etc/docker
cat >/etc/docker/daemon.json <<'DAEMONEOF'
{"hosts":["unix:///var/run/docker.sock","tcp://0.0.0.0:@DOCKER_PORT@"],"tlsverify":true,"tlscacert":"/etc/docker/tls/ca.pem","tlscert":"/etc/docker/tls/server.pem","tlskey":"/etc/docker/tls/server-key.pem","storage-driver":"overlay2"}
DAEMONEOF

# Стартуем dockerd напрямую: OpenRC-сервис docker зависит от сервиса
# sysfs, который в netboot-окружении остаётся в failed-состоянии.
/usr/bin/dockerd --config-file=/etc/docker/daemon.json >/var/log/dockerd.log 2>&1 &

# Агент boxlet shell (приезжает внутри apkovl, если собран на хосте).
if [ -x /usr/local/bin/boxlet-agent ]; then
    /usr/local/bin/boxlet-agent >/var/log/boxlet-agent.log 2>&1 &
fi

# Ждём готовности и показываем диагностику в консоль.
i=0
while [ $i -lt 60 ]; do
    docker info >/dev/null 2>&1 && break
    sleep 1
    i=$((i+1))
done
if docker info >/dev/null 2>&1; then
    echo "boxlet: dockerd запущен"
    # Мини-образ со статическим busybox для служебных операций с хоста:
    # sync и штатное выключение гостевой ОС (см. backend.SyncGuest/PowerOffGuest).
    mkdir -p /tmp/boxlet-img
    cp /bin/busybox.static /tmp/boxlet-img/busybox
    (cd /tmp/boxlet-img && tar -cf - busybox) | docker import - boxlet-agent:latest >/dev/null 2>&1 \
        && echo "boxlet: образ boxlet-agent готов"
else
    echo "boxlet: dockerd НЕ запустился, последние строки лога:"
    tail -30 /var/log/dockerd.log
fi
echo "boxlet bootstrap: done"
`

// rosettaScript — включение эмуляции linux/amd64 через Rosetta
// (только darwin-vz на Apple Silicon; virtiofs-шара "rosetta").
const rosettaScript = `
# Rosetta: эмуляция linux/amd64 (Apple Silicon, darwin-vz).
modprobe binfmt_misc || true
mountpoint -q /proc/sys/fs/binfmt_misc || mount -t binfmt_misc none /proc/sys/fs/binfmt_misc || true
mkdir -p /run/rosetta
if mount -t virtiofs rosetta /run/rosetta 2>/dev/null; then
    echo ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/run/rosetta/rosetta:OCF' \
        > /proc/sys/fs/binfmt_misc/register
    echo "boxlet: rosetta для linux/amd64 включена"
else
    echo "boxlet: шара rosetta недоступна, эмуляция amd64 отключена"
fi
`

// shutdownScript — /etc/local.d/boxlet.stop: при штатном выключении
// (хост шлёт USR2 в init гостя → poweroff) OpenRC останавливает сервис
// local, и мы успеваем корректно погасить dockerd и сбросить data-диск
// до mount-ro.
const shutdownScript = `#!/bin/sh
exec >/dev/console 2>&1
echo "boxlet shutdown: останавливаю dockerd"
pid=$(cat /var/run/docker.pid 2>/dev/null)
if [ -n "$pid" ]; then
    kill -TERM "$pid" 2>/dev/null
    i=0
    while kill -0 "$pid" 2>/dev/null && [ $i -lt 20 ]; do
        sleep 1
        i=$((i+1))
    done
fi
sync
umount /var/lib/docker 2>/dev/null || true
echo "boxlet shutdown: done"
`

// DockerPort — TCP-порт dockerd (только TLS с клиентским сертификатом)
// внутри гостевой ОС. qemu пробрасывает на него порт docker_port
// с 127.0.0.1 хоста, darwin-vz подключается к нему по IP гостя в vmnet.
// Номер прежний (2375), чтобы старые конфиги с docker_port: 2375
// продолжали работать; 2376 занят гостевым агентом (AgentPort).
const DockerPort = 2375

const networkInterfaces = `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
`

// tarFile — один элемент оверлея apkovl.
type tarFile struct {
	name     string
	body     []byte
	mode     int64
	linkname string // для симлинков
}

// BuildApkovl генерирует apkovl.tar.gz — оверлей /etc для diskless-загрузки
// Alpine: сеть, репозитории, TLS-сертификаты dockerd (tls), автозапуск
// сервиса local со скриптами запуска и остановки.
// mounts — каталоги хоста для virtiofs-шар (darwin-vz); rosetta — включить
// эмуляцию linux/amd64 через Rosetta; agent — статический бинарь
// гостевого агента (boxlet shell), nil — shell будет недоступен;
// token — токен аутентификации агента (см. AgentToken).
func BuildApkovl(mirror, release string, mounts []string, rosetta bool, agent []byte, token string, tls *certs.Bundle) ([]byte, error) {
	repositories := fmt.Sprintf("%s/%s/main\n%s/%s/community\n", mirror, release, mirror, release)

	// Секция шар хоста и Rosetta для bootstrap-скрипта.
	var extra strings.Builder
	if len(mounts) > 0 {
		extra.WriteString("\n# virtiofs-шары хоста (darwin-vz): тот же абсолютный путь,\n")
		extra.WriteString("# что на хосте, чтобы работал docker run -v <host-path>.\n")
		extra.WriteString("modprobe virtiofs || true\n")
		for i, m := range mounts {
			fmt.Fprintf(&extra, "mkdir -p %q\n", m)
			fmt.Fprintf(&extra, "mount -t virtiofs share%d %q || echo \"boxlet: шара share%d (%s) недоступна\"\n", i, m, i, m)
		}
	}
	if rosetta {
		extra.WriteString(rosettaScript)
	}
	script := fmt.Sprintf(bootstrapScript, extra.String())
	script = strings.ReplaceAll(script, "@DOCKER_PORT@", strconv.Itoa(DockerPort))

	files := []tarFile{
		{name: "etc/network/", mode: 0o755},
		{name: "etc/network/interfaces", body: []byte(networkInterfaces), mode: 0o644},
		{name: "etc/apk/", mode: 0o755},
		{name: "etc/apk/repositories", body: []byte(repositories), mode: 0o644},
		{name: "etc/init.d/", mode: 0o755},
		{name: "etc/runlevels/", mode: 0o755},
		{name: "etc/runlevels/default/", mode: 0o755},
		{name: "etc/runlevels/default/local", mode: 0o777, linkname: "/etc/init.d/local"},
		{name: "etc/runlevels/boot/", mode: 0o755},
		{name: "etc/runlevels/boot/networking", mode: 0o777, linkname: "/etc/init.d/networking"},
		{name: "etc/local.d/", mode: 0o755},
		{name: "etc/local.d/boxlet.start", body: []byte(script), mode: 0o755},
		{name: "etc/local.d/boxlet.stop", body: []byte(shutdownScript), mode: 0o755},
		{name: "etc/docker/", mode: 0o755},
		{name: "etc/docker/tls/", mode: 0o700},
		{name: "etc/docker/tls/ca.pem", body: tls.CA, mode: 0o644},
		{name: "etc/docker/tls/server.pem", body: tls.ServerCert, mode: 0o644},
		{name: "etc/docker/tls/server-key.pem", body: tls.ServerKey, mode: 0o600},
	}
	if len(agent) > 0 {
		files = append(files,
			tarFile{name: "usr/local/bin/", mode: 0o755},
			tarFile{name: "usr/local/bin/boxlet-agent", body: agent, mode: 0o755},
			// Токен агента: только root гостевой ОС.
			tarFile{name: "etc/boxlet-agent.token", body: []byte(token), mode: 0o600},
		)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

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
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(f.body); err != nil {
				return nil, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
