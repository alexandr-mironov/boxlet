# Совместимость

Boxlet предоставляет стандартный Docker Engine API и совместим
с инструментами, которые ожидают «настоящий» Docker.

Каждый пункт ниже проверяется командой `boxlet doctor`.

## 1. Docker Engine API

- [x] `docker version` проходит, актуальный API (Docker Engine 29)
- [x] поддерживаются hijack/upgrade-соединения (`docker run -it`,
      `docker exec`, attach-потоки, testcontainers)

## 2. Сокет

- [x] `~/.boxlet/run/docker.sock` слушает и отвечает
- [ ] `/var/run/docker.sock` — симлинк на него (создаётся `boxlet start`
      через sudo или вручную по подсказке из `boxlet doctor`)

## 3. Сети

- [x] сети из `auto_networks` в конфиге создаются автоматически при старте
- [ ] проброс портов контейнеров на localhost: сейчас через список
      `forwarded_ports` в `~/.boxlet/config.json` (порты <1024 требуют
      запуска от root — ограничение qemu user-mode networking)

## 4. Реестры

- [x] `~/.docker/config.json` читается docker CLI как обычно
      (boxlet не подменяет DOCKER_CONFIG)

## 5. Архитектуры

- [ ] образы linux/amd64 на Apple Silicon: в M2 через Rosetta в гостевой ОС;
      на qemu-бэкенде возможен запуск amd64-образов через эмуляцию qemu
      (медленно, не настроено по умолчанию)

## 6. Доступ к файлам

- [ ] bind-mount каталогов хоста (`docker run -v $PWD:/src ...`) —
      в M2 через virtiofs для macOS; на qemu-бэкенде пока недоступно

## Известные ограничения qemu-бэкенда (M1)

- проброс портов только статическим списком, без <1024 без root
- нет bind-mount с хоста (данные контейнеров живут в VM на qcow2-диске)
- нет shell в VM из CLI (обходной путь: privileged-контейнер + nsenter)
