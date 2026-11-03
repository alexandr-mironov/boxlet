package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"time"
)

// bootstrapScript — скрипт первой загрузки гостевой ОС.
// Запускается сервисом local (OpenRC) из /etc/local.d/boxlet.start:
// ставит docker из репозиториев Alpine, форматирует и монтирует
// data-диск под /var/lib/docker, настраивает и запускает dockerd.
const bootstrapScript = `#!/bin/sh
# boxlet: автонастройка гостевой ОС (генерируется, не редактировать)
# Лог в консоль (qemu-serial.log).
exec >/dev/console 2>&1
set -x
echo "boxlet bootstrap: start"

# Подстраховка сети: если интерфейс без адреса — dhcp вручную.
if ! ip -4 addr show dev eth0 | grep -q "inet "; then
    ip link set eth0 up
    udhcpc -i eth0 -q
fi

apk update
apk add docker e2fsprogs

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

# dockerd слушает и unix-сокет, и TCP — на TCP ходит прокси с хоста
# (доступен только через NAT-проброс qemu на 127.0.0.1).
mkdir -p /etc/docker
cat >/etc/docker/daemon.json <<'DAEMONEOF'
{"hosts":["unix:///var/run/docker.sock","tcp://0.0.0.0:2375"],"storage-driver":"overlay2"}
DAEMONEOF

# Стартуем dockerd напрямую: OpenRC-сервис docker зависит от сервиса
# sysfs, который в netboot-окружении остаётся в failed-состоянии.
/usr/bin/dockerd --config-file=/etc/docker/daemon.json >/var/log/dockerd.log 2>&1 &

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
    # sync файловых систем перед остановкой VM (см. backend Stop).
    mkdir -p /tmp/boxlet-agent
    cp /bin/busybox /tmp/boxlet-agent/
    (cd /tmp/boxlet-agent && tar -cf - busybox) | docker import - boxlet-agent:latest >/dev/null 2>&1 \
        && echo "boxlet: образ boxlet-agent готов"
else
    echo "boxlet: dockerd НЕ запустился, последние строки лога:"
    tail -30 /var/log/dockerd.log
fi
echo "boxlet bootstrap: done"
`

const networkInterfaces = `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
`

// BuildApkovl генерирует apkovl.tar.gz — оверлей /etc для diskless-загрузки
// Alpine: сеть, репозитории, автозапуск сервиса local с bootstrap-скриптом.
func BuildApkovl(mirror, release string) ([]byte, error) {
	repositories := fmt.Sprintf("%s/%s/main\n%s/%s/community\n", mirror, release, mirror, release)

	files := []struct {
		name     string
		body     string
		mode     int64
		linkname string // для симлинков
	}{
		{name: "etc/network/", mode: 0o755},
		{name: "etc/network/interfaces", body: networkInterfaces, mode: 0o644},
		{name: "etc/apk/", mode: 0o755},
		{name: "etc/apk/repositories", body: repositories, mode: 0o644},
		{name: "etc/init.d/", mode: 0o755},
		{name: "etc/runlevels/", mode: 0o755},
		{name: "etc/runlevels/default/", mode: 0o755},
		{name: "etc/runlevels/default/local", mode: 0o777, linkname: "/etc/init.d/local"},
		{name: "etc/runlevels/boot/", mode: 0o755},
		{name: "etc/runlevels/boot/networking", mode: 0o777, linkname: "/etc/init.d/networking"},
		{name: "etc/local.d/", mode: 0o755},
		{name: "etc/local.d/boxlet.start", body: bootstrapScript, mode: 0o755},
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
		case f.body == "" && f.name[len(f.name)-1] == '/':
			hdr.Typeflag = tar.TypeDir
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, f.body); err != nil {
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
