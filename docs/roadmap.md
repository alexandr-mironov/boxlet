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

- [ ] helper `boxlet-vm` на Virtualization.framework (Swift)
- [ ] vsock-канал вместо TCP для прокси сокета
- [ ] Rosetta для linux/amd64
- [ ] virtiofs-шаринг каталогов хоста (bind-mount в контейнеры)
- [ ] прозрачный проброс портов, включая <1024 (privileged helper)

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
