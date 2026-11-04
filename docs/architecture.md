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

### darwin-vz (macOS, работает, M2)

Виртуализация через `Virtualization.framework`.
Go не имеет нативных биндингов, поэтому VM-процесс — отдельный
helper-бинарь `boxlet-vm` (Swift, тонкая обёртка над VZ, собирается
`make vm-helper`, ad-hoc подпись с entitlement
`com.apple.security.virtualization`), которым `boxletd` управляет
как дочерним процессом:

- загрузка та же, что у qemu-бэкенда (diskless Alpine, apkovl по HTTP),
  но оверлей гость забирает со шлюза vmnet (адрес из
  `com.apple.vmnet.plist`, обычно 192.168.64.1)
- ядро: Alpine публикует aarch64-ядро в обёртке EFI zboot, а VZ принимает
  только несжатый Image — `guest.EnsureVMLinux` извлекает его
  (gzip-поток внутри PE, сигнатура `ARM\x64`) в `vmlinux-virt`
- сеть: NAT через vmnet; IP гостя находится по MAC в
  `/var/db/dhcpd_leases` (октеты MAC там без ведущих нулей, порядок полей
  в записи не гарантирован); dockerd доступен хосту напрямую по
  `guestIP:2375`, hostfwd не нужен
- Rosetta для linux/amd64 (`VZLinuxRosettaDirectoryShare`, тег rosetta,
  в гостевой ОС регистрируется binfmt_misc)
- virtiofs-шары каталогов из `mounts`: в гостевой ОС монтируются по тем
  же путям, что на хосте, поэтому bind-mount `-v ~/projects/...` в
  контейнеры работает прозрачно
- диск данных — разреженный raw-образ (VZ не читает qcow2)
- штатная остановка: SIGTERM хелперу → `requestStop()` в VZ
- проброс портов: dockerd публикует порты контейнеров на 0.0.0.0 гостя;
  демон (`internal/proxy.PortForwarder`) опрашивает dockerd раз в 2 секунды
  и поднимает слушатели 127.0.0.1:port → guestIP:port для всех
  опубликованных TCP-портов + статического списка `forwarded_ports`;
  проброс снимается при остановке контейнера. IP гостя разрешается
  на каждое соединение (может смениться между запусками VM).
  Порты <1024 слушает setuid-хелпер `boxlet-priv` (cmd/boxlet-priv):
  устанавливается `boxlet priv install` (root:wheel + setuid через
  osascript с GUI-запросом пароля), биндит порты из `forwarded_ports`
  и сбрасывает привилегии обратно к пользователю; он же создаёт
  симлинк /var/run/docker.sock (операция `link`)

- shell в гостевую ОС: статический агент boxlet-agent (cmd/boxlet-agent)
  едет в VM внутри apkovl и слушает :2376; `boxlet shell` соединяется
  напрямую по IP гостя (одно соединение = одна команда, дальше сырой
  stdio). На qemu-бэкенде агент доступен через hostfwd 127.0.0.1:2376.
  Протокол: первая строка — токен (~/.boxlet/run/agent-token, генерируется
  на хосте при старте, едет в apkovl как /etc/boxlet-agent.token с правами
  0600, сравнение constant-time), вторая — argv через \x00. Агент без
  верного токена соединение закрывает: порт 2376 доступен любому процессу
  хоста, поэтому без аутентификации это был бы root в гостевой ОС
- DNS-зона *.boxlet.local: минимальный сервер в boxletd
  (internal/dnsd, 127.0.0.1:15353, только stdlib) отвечает на A-запросы
  текущим IP гостя, TTL 5 сек; macOS направляет зону на него файлом
  /etc/resolver/boxlet — ставится `boxlet dns install` (через операцию
  `resolver` хелпера boxlet-priv: путь и контент файла фиксированы)
- усыновление VM: если boxletd перезапустился, а boxlet-vm жив, демон
  подхватывает её (pid-файл + проверка процесса по ps), а не падает
  с «уже запущен»; boxletd — синглтон (второй экземпляр не стартует
  при занятом порте API)

Запланировано: vsock для гостевого агента и прокси сокета, pty в shell.

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
