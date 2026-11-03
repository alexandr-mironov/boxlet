# Архитектура Boxlet

## Обзор

Boxlet состоит из трёх процессных уровней:

1. **CLI (`boxlet`)** — тонкий клиент. Почти все команды транслирует в
   демон через HTTP API. Команды-исключения (`init`, `doctor`) работают
   локально.
2. **Демон (`boxletd`)** — ядро. Живёт в фоне, управляет жизненным циклом
   бэкенда (VM или нативного рантайма), поднимает прокси Docker API
   и отдаёт веб-интерфейс.
3. **Бэкенд** — то, где реально крутятся контейнеры. Абстрагирован
   интерфейсом `backend.Backend`, реализации выбираются по платформе
   и конфигурации.

## Интерфейс бэкенда

```go
type Backend interface {
    Name() string
    // Prepare: одноразовая подготовка (образ VM, директории, права)
    Prepare(ctx context.Context) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Status(ctx context.Context) (Status, error)
    // GuestSocket: соединение с docker API внутри бэкенда,
    // куда должен проксировать socket proxy
    GuestSocket(ctx context.Context) (net.Conn, error)
    // Exec: выполнить команду внутри бэкенда (диагностика, shell)
    Exec(ctx context.Context, cmd ...string) ([]byte, error)
}
```

Новый драйвер = одна реализация интерфейса. Драйвер выбирается:
явно в конфиге (`backend: qemu`), иначе — дефолт платформы.

## Драйверы

### qemu (работает, основной на M1)

Diskless Alpine Linux в qemu:

- загрузка по netboot: ядро и initramfs скачиваются с зеркала Alpine
- `modloop-virt` подключается отдельным read-only virtio-диском —
  из него гостевая ОС получает модули ядра (overlayfs, ext4)
- настройка через apkovl-оверлей (генерируется при старте, отдаётся
  в VM по HTTP через slirp-адрес хоста 10.0.2.2): сеть, репозитории,
  bootstrap-скрипт в /etc/local.d
- bootstrap ставит `docker` из apk, монтирует qcow2-диск под
  /var/lib/docker, поднимает dockerd на unix-сокете и TCP 2375
- qemu user-mode networking: hostfwd пробрасывает 2375 (dockerd)
  и порты из `forwarded_ports` на localhost
- ускорение: hvf на macOS, kvm на Linux

### darwin-vz (macOS, целевой, M2)

Виртуализация через `Virtualization.framework`.
Go не имеет нативных биндингов, поэтому VM-процесс — отдельный
helper-бинарь `boxlet-vm` (Swift, тонкая обёртка над VZ), которым
`boxletd` управляет как дочерним процессом:

- vsock для гостевого агента и проксирования сокета
- Rosetta для linux/amd64 (`VZLinuxRosettaDirectoryShare`)
- virtiofs для шаринга каталогов проектов в VM
- virtio-net; прозрачный проброс портов

### wsl2 (Windows, M3)

Managed-дистрибутив WSL2 (`wsl --import` rootfs с dockerd),
прокси сокета через `//./pipe/` + AF_VSOCK.

### linux (нативный)

Без VM: rootless или системный dockerd, управление через systemd.
Сокет уже на месте, прокси не нужен — включается passthrough-режим.

## Прокси сокета

Критичный элемент совместимости: на хосте создаётся
`~/.boxlet/run/docker.sock`, опционально симлинк
`/var/run/docker.sock` → него. Прокси — L4: принимает соединения на
unix-сокете и пробрасывает в гостевой dockerd, не разбирая протокол.
Это даёт полную совместимость включая attach/hijack-стримы
(`docker run -it`, `docker exec`, testcontainers).

Тонкость: EOF в одну сторону (half-close, например docker CLI закрывает
stdin после attach-запроса) пробрасывается через CloseWrite, оба
направления дожидаются друг друга — иначе теряется вывод контейнера.

## Веб-интерфейс (GUI MVP)

`boxletd` слушает `http://127.0.0.1:9770` и отдаёт SPA (embedded FS):
список контейнеров, образы, кнопки start/stop, статус VM.
Данные — проксирование вызовов Docker API + собственный `/api/status`.
Полноценный desktop GUI планируется поверх того же API демона
(см. docs/roadmap.md).

## Директории

```
~/.boxlet/
  config.json          # конфигурация инстанса
  run/
    boxletd.pid        # pid демона
    qemu.pid           # pid VM
    docker.sock        # публичный Docker API сокет
  images/              # netboot-файлы Alpine (kernel, initramfs, modloop)
  data/                # qcow2-диск под данные dockerd
  logs/                # логи демона, qemu и серийной консоли VM
```
