# Роадмап

## M0 — каркас

- [x] CLI: `init`, `start`, `stop`, `status`, `doctor`, `shell`, `ps`
- [x] демон `boxletd` с control API
- [x] интерфейс бэкенда + заглушки драйверов (qemu, darwin-vz, wsl2, linux)
- [x] прокси docker-сокета (L4)
- [x] веб-интерфейс MVP (статус, контейнеры, start/stop)
- [x] `boxlet doctor` — проверка окружения

## M1 — рабочий qemu-бэкенд

- [x] скачивание Alpine netboot (kernel/initramfs/modloop) с зеркала
- [x] diskless-загрузка VM: apkovl-оверлей по HTTP, modloop диском
- [x] автонастройка гостевой ОС: docker из apk, dockerd на unix+tcp
- [x] qcow2-диск под /var/lib/docker (образы переживают перезапуск)
- [x] проброс порта dockerd на localhost, L4-прокси сокета
- [x] hijack-совместимость: `docker run -it`, attach, `docker exec`
- [x] автосоздание сетей из `auto_networks`, symlink `/var/run/docker.sock`
- [ ] проброс произвольных портов контейнеров (сейчас — статический список)

## M2 — macOS native

- [x] helper `boxlet-vm` на Virtualization.framework (Swift, ad-hoc подпись
      с entitlement; распаковка ядра из zboot-обёртки Alpine)
- [x] NAT через vmnet: IP гостя из /var/db/dhcpd_leases, dockerd доступен
      хосту напрямую (hostfwd не нужен)
- [x] Rosetta для linux/amd64 (`--platform linux/amd64` работает)
- [x] virtiofs-шаринг каталогов хоста (bind-mount в контейнеры,
      монтируются по тем же путям, что на хосте)
- [x] прозрачный проброс портов на 127.0.0.1: userspace-форвардер следит
      за публикациями контейнеров (опрос dockerd) + статический список
      `forwarded_ports`; порты <1024 — через setuid-хелпер `boxlet-priv`
      (`boxlet priv install`), демон передаёт ему актуальный список
      (включая публикации контейнеров) и перезапускает при изменении
- [x] `boxlet shell`: гостевой агент boxlet-agent (:2376, едет в apkovl),
      работает и на darwin-vz (по IP гостя), и на qemu (hostfwd)
- [x] усыновление живой VM при перезапуске демона + синглтон boxletd
      (второй экземпляр не стартует при занятом порте API)
- [x] `boxlet set` (CPU/RAM/диск), расход ресурсов в `boxlet status`
- [x] `boxlet service install` — автозапуск boxletd (LaunchAgent, macOS)
- [x] `boxlet upgrade` — перекат гостевой ОС на свежий Alpine
      (диск данных сохраняется)
- [x] токен-аутентификация гостевого агента (генерируется на хосте,
      едет в apkovl с правами 0600; constant-time сравнение)
- [x] `boxlet logs` — логи демона, VM, хелпера, dockerd и агента
      (локальные файлы + хвосты из гостя, -f для слежения)
- [x] live-метрики гостя в `boxlet status` (load average, RAM)
- [x] версия из git (ldflags, `boxlet version`)
- [x] unit-тесты (vmnet leases, zboot-распаковка, apkovl, порты, DNS)
      + CI на GitHub Actions (macos-latest: vet, тесты, make build)
- [x] DNS-зона `*.boxlet.local` → адрес гостевой ОС: сервер в boxletd
      (127.0.0.1:15353), активация через /etc/resolver/boxlet
      (`boxlet dns install`)
- [x] веб-интерфейс: live-метрики гостя (load, RAM), кнопка «Рестарт»
      (`POST /api/restart`), просмотр логов (`GET /api/logs`) со слежением;
      фикс гонки Stop→Start (reap зомби-процесса boxlet-vm)
- [ ] vsock-канал вместо TCP для прокси сокета и агента
- [ ] pty в boxlet shell (сейчас строчный режим без pty)

## M3 — Windows

- [ ] wsl2-драйвер, named pipe прокси
- [ ] инсталлятор

## M4 — GUI

- [ ] desktop-приложение поверх API демона
- [ ] менюбар-трей: статус, start/stop, расход ресурсов

## M5 — миграция с Docker Desktop / OrbStack

- [ ] импорт образов и томов (`docker save/load` пайплайн)
- [ ] мастер переключения DOCKER_HOST/контекстов
- [ ] режим side-by-side (не ломать существующий Docker)
