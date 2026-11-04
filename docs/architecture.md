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
  в VM по HTTP через slirp-адрес хоста 10.0.2.2; сервер слушает только
  127.0.0.1, отдаёт оверлей по случайному пути из kernel cmdline
  и закрывается после готовности dockerd): сеть, репозитории,
  бинарь агента, скрипты запуска и остановки в /etc/local.d.
  Секретов в оверлее нет — см. «Секреты гостевой ОС»
- bootstrap ставит `docker` из apk, монтирует qcow2-диск под
  /var/lib/docker, поднимает dockerd на unix-сокете и TCP 2375
  только с `tlsverify`
- qemu user-mode networking: hostfwd пробрасывает TLS-порт dockerd
  на `127.0.0.1:<docker_port>` и порты из `forwarded_ports` на localhost
- `Stop` во время загрузки прерывает `Start`; смерть qemu при загрузке
  обнаруживается сразу, а не по таймауту
- если демон упал, а VM осталась (pid из `run/qemu.pid`; процесс
  считается своим, только если в его командной строке наш диск данных
  и наш pidfile), следующий `start` подхватывает её без
  перезагрузки, если dockerd отвечает, иначе выключает и грузит заново
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
  `guestIP:2375` — по TLS с клиентским сертификатом, как у qemu;
  hostfwd не нужен
- netboot-сервер слушает все интерфейсы (мост vmnet появляется только
  вместе с VM); секретов в оверлее нет, сервер закрывается после загрузки
- Rosetta для linux/amd64 (`VZLinuxRosettaDirectoryShare`, тег rosetta,
  в гостевой ОС регистрируется binfmt_misc)
- virtiofs-шары каталогов из `mounts`: в гостевой ОС монтируются по тем
  же путям, что на хосте, поэтому bind-mount `-v ~/projects/...` в
  контейнеры работает прозрачно
- диск данных — разреженный raw-образ (VZ не читает qcow2)
- остановка — общая для обоих бэкендов, см. ниже; SIGTERM хелперу →
  `requestStop()` в VZ — запасной путь
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

## Секреты гостевой ОС

TLS-пара dockerd и токен агента не кладутся в apkovl: URL оверлея
передаётся в kernel cmdline, а её печатает серийная консоль (логи VM,
`/api/logs?name=vm`) и видно в аргументах процесса VM. Вместо этого
`guest.WriteSecrets` пишет несжатый tar в `~/.boxlet/run/guest-secrets/`
(каталог 0700, файл 0600), и гость получает его по каналу, недоступному
с хоста посторонним:

- qemu: `-fw_cfg name=opt/boxlet/secrets,file=…` — в аргументах виден
  только путь; в госте `/sys/firmware/qemu_fw_cfg/by_name/…/raw`
  (читает только root)
- darwin-vz: virtiofs-шара `boxlet-secrets` этого каталога, монтируется
  read-only на время распаковки

Без секретов dockerd с `tlsverify` и агент не запускаются (агент без
токена завершается сам).

## Доступ к dockerd и остановка VM (оба VM-бэкенда)

- dockerd в госте слушает TCP только с `tlsverify`: CA, серверный
  и клиентский сертификаты выпускаются в `~/.boxlet/certs` (0700) при
  первом запуске, в гостевую ОС едут только CA и серверная пара. Без
  клиентского сертификата к порту dockerd не подключиться ни другому
  локальному процессу, ни другой VM в vmnet, ни веб-странице через
  DNS rebinding. Публичный `~/.boxlet/run/docker.sock` — 0600
- остановка: `sync` и затем USR2 в init гостя через гостевой агент
  (не зависит от хранилища образов Docker — `docker system prune -a`
  ему не мешает); если агента в VM нет — через служебный контейнер
  `boxlet-agent` (`--pid=host`, статический busybox). Это штатный
  poweroff с `openrc shutdown`, `local.d/boxlet.stop` гасит dockerd
  и отмонтирует data-диск. SIGTERM/SIGKILL процесса VM — только если гость не
  выключился за 30 с. (`system_powerdown` в qemu не подходит: в Alpine
  virt-ядре на arm нет gpio-keys, гость кнопку питания не видит)

## Директории

```
~/.boxlet/
  config.json          # конфигурация инстанса
  run/
    boxletd.pid        # pid демона
    qemu.pid           # pid VM
    docker.sock        # публичный Docker API сокет (0600)
  certs/               # TLS-комплект для dockerd в VM (0700)
  images/              # netboot-файлы Alpine (kernel, initramfs, modloop)
  data/                # qcow2-диск под данные dockerd
  logs/                # логи демона, qemu и серийной консоли VM
```
