# Boxlet

Лёгкий менеджер контейнеров — альтернатива Docker Desktop и OrbStack.

Ключевой принцип — **максимальная совместимость**: boxlet поднимает
стандартный Docker Engine API, поэтому существующие инструменты
(`docker` CLI, `docker compose`, testcontainers и любые клиенты
Docker API) работают без изменений, а переход с Docker Desktop / OrbStack
занимает минуты.

## Как это работает

```
┌─────────────────────────────┐
│  boxlet CLI / GUI (web ui)  │
└──────────────┬──────────────┘
               │
┌──────────────▼──────────────┐
│  boxletd (демон-суревизор)  │
│  ├─ socket proxy            │◄── ~/.boxlet/run/docker.sock
│  │     (/var/run/docker.sock├── symlink для совместимости)
│  └─ backend driver          │
└──────────────┬──────────────┘
               │
┌──────────────▼──────────────┐
│  Linux VM / native runtime  │
│  └─ dockerd (Moby)          │
└─────────────────────────────┘
```

Бэкенды (драйверы запуска контейнерного рантайма):

| Платформа | Драйвер | Статус |
|---|---|---|
| macOS (Apple Silicon) | `darwin-vz` — Virtualization.framework через helper | работает: Rosetta (linux/amd64), virtiofs bind-mount |
| macOS / Linux | `qemu` — diskless Alpine VM с dockerd | работает |
| Windows | `wsl2` — managed WSL2-дистрибутив | каркас |
| Linux | `linux` — нативный dockerd без VM | каркас |

Выбор бэкенда — поле `backend` в `~/.boxlet/config.json`
(`darwin-vz`, `qemu`). На Apple Silicon рекомендуется `darwin-vz`:
загрузка за секунды, Rosetta и bind-mount каталогов хоста.

Внутри VM разворачивается минимальный diskless Alpine Linux с `dockerd`,
т.е. API и поведение полностью совпадают с «настоящим» Docker.

## Быстрый старт

```sh
# зависимости: qemu (brew install qemu) — только для qemu-бэкенда;
# darwin-vz собирается штатным swiftc из Xcode CLT
make build   # на macOS также собирает и подписывает bin/boxlet-vm

# инициализация (создаёт ~/.boxlet, скачивает образы Alpine)
boxlet init

# запуск (первая загрузка VM — несколько минут)
boxlet start

# проверка окружения
boxlet doctor

# дальше — обычный docker
export DOCKER_HOST=unix://$HOME/.boxlet/run/docker.sock
docker ps
```

## Структура репозитория

- `cmd/boxlet` — CLI (init/start/stop/status/doctor/shell/set/service/upgrade/ps/...)
- `cmd/boxletd` — демон: управляет VM, сокет-прокси, веб-интерфейсом
- `cmd/boxlet-agent` — агент в гостевой ОС для `boxlet shell` (linux, static)
- `cmd/boxlet-priv` — setuid-хелпер: порты <1024, symlink /var/run/docker.sock
- `internal/backend` — интерфейс драйвера + реализации per-платформа
- `internal/guest` — гостевая ОС: скачивание Alpine, apkovl-оверлей
- `internal/proxy` — проксирование Docker API сокета из VM на хост
- `internal/config`, `internal/paths` — конфигурация и пути инстанса
- `internal/webui` — встроенный веб-интерфейс (GUI MVP)
- `vmhelper/` — Swift-хелпер `boxlet-vm` (Virtualization.framework)
  и entitlements для ad-hoc подписи
- `docs/` — архитектура, роадмап, заметки по совместимости

## Документация

- [Архитектура](docs/architecture.md)
- [Совместимость](docs/compat.md)
- [Роадмап](docs/roadmap.md)
