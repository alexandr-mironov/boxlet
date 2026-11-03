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
| macOS (arm64/amd64) | `darwin-vz` — Virtualization.framework через helper | каркас |
| macOS / Linux | `qemu` — diskless Alpine VM с dockerd | работает |
| Windows | `wsl2` — managed WSL2-дистрибутив | каркас |
| Linux | `linux` — нативный dockerd без VM | каркас |

Внутри VM разворачивается минимальный diskless Alpine Linux с `dockerd`,
т.е. API и поведение полностью совпадают с «настоящим» Docker.

## Быстрый старт

```sh
# зависимости: qemu (brew install qemu)
make build

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

- `cmd/boxlet` — CLI (init/start/stop/status/doctor/shell/ps/...)
- `cmd/boxletd` — демон: управляет VM, сокет-прокси, веб-интерфейсом
- `internal/backend` — интерфейс драйвера + реализации per-платформа
- `internal/guest` — гостевая ОС: скачивание Alpine, apkovl-оверлей
- `internal/proxy` — проксирование Docker API сокета из VM на хост
- `internal/config`, `internal/paths` — конфигурация и пути инстанса
- `internal/webui` — встроенный веб-интерфейс (GUI MVP)
- `docs/` — архитектура, роадмап, заметки по совместимости

## Документация

- [Архитектура](docs/architecture.md)
- [Совместимость](docs/compat.md)
- [Роадмап](docs/roadmap.md)
