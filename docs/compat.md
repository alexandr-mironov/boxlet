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

- [x] образы linux/amd64 на Apple Silicon: на бэкенде darwin-vz через
      Rosetta в гостевой ОС (проверено: `--platform linux/amd64`);
      на qemu-бэкенде возможна эмуляция qemu (медленно, не настроено)

## 6. Доступ к файлам

- [x] bind-mount каталогов хоста (`docker run -v $PWD:/src ...`) —
      на бэкенде darwin-vz через virtiofs (каталоги из `mounts` в конфиге
      видны в гостевой ОС по тем же путям); на qemu-бэкенде недоступно

## Известные ограничения darwin-vz (M2)

- проброс портов контейнеров на localhost не реализован: порты доступны
  напрямую по IP гостя (`boxlet status` показывает его), проброс на
  localhost, включая <1024, — в планах (privileged helper)
- нет shell в VM из CLI (обходной путь: privileged-контейнер + nsenter)

## Известные ограничения qemu-бэкенда (M1)

- проброс портов только статическим списком, без <1024 без root
- нет bind-mount с хоста (данные контейнеров живут в VM на qcow2-диске)
- нет shell в VM из CLI (обходной путь: privileged-контейнер + nsenter)
