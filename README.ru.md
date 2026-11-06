# Boxlet

**English** | [Русский](README.ru.md)

Лёгкий менеджер контейнеров — **бесплатная** альтернатива Docker Desktop
и OrbStack с открытым исходным кодом.

Ключевой принцип — **максимальная совместимость**: внутри VM работает
настоящий Docker Engine (`dockerd`), поэтому существующие инструменты
(`docker` CLI, `docker compose`, testcontainers и любые клиенты
Docker API) работают без изменений, а переход с Docker Desktop / OrbStack
занимает минуты. Boxlet бесплатен для всех, включая коммерческое
использование — без аккаунтов, подписок и телеметрии (Apache 2.0,
см. [LICENSE](LICENSE) и [NOTICE](NOTICE)).

## Возможности

- **Быстрый старт на Apple Silicon** — VM на Virtualization.framework
  грузится за секунды, без стороннего гипервизора
- **Настоящий dockerd** — полная совместимость API, включая
  `docker run -it`, attach и testcontainers
- **Rosetta** — образы `linux/amd64` работают из коробки
- **Bind-mount каталогов хоста** через virtiofs: `-v ~/projects:/src`
  просто работает
- **Прозрачный проброс портов** контейнеров на `127.0.0.1`,
  включая порты <1024 (через `boxlet priv install`)
- **mTLS к dockerd** — к Docker API в VM не подключиться без клиентского
  сертификата (выпускается автоматически при первом запуске)
- **Имена `*.boxlet.local`** для опубликованных портов (`boxlet dns install`)
- **Менюбар-трей** (`boxlet tray`) и **веб-интерфейс** (статус, контейнеры,
  live-метрики, логи, рестарт)
- **CLI полного цикла**: `init`, `start`, `stop`, `status`, `logs`,
  `shell`, `set` (ресурсы VM), `service` (автозапуск), `upgrade`
- **Гостевой агент с токен-аутентификацией** для shell и диагностики
- Штатная остановка гостевой ОС (sync + poweroff), данные на диске
  переживают перезапуски и обновления

## Как это работает

```
┌─────────────────────────────┐
│ CLI / веб-интерфейс / трей  │
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

### 1. Установка из готового релиза (macOS, Apple Silicon)

```sh
curl -fsSL https://raw.githubusercontent.com/alexandr-mironov/boxlet/main/install.sh | bash
```

Одна команда и для первой установки, и для обновления. Скрипт скачивает
последний релиз и проверяет sha256, штатно останавливает работающий
движок, кладёт бинари в `~/.boxlet/bin`, создаёт конфиг (существующий
не трогает), запускает движок и показывает `boxlet doctor`. Пароль
администратора спрашивается один раз и только когда нужен: для симлинка
`/usr/local/bin/boxlet` и для privileged helper (порты <1024), если он
уже стоял или в конфиге есть такие порты. С `BOXLET_NO_SUDO=1` эти шаги
пропускаются, а `~/.boxlet/bin` дописывается в `PATH` в профиле shell;
`BOXLET_NO_START=1` — не запускать движок.

После установки из релиза шаг 2 ниже уже выполнен.

### 1-alt. Установка из исходников

```sh
# зависимости: qemu (brew install qemu) — только для qemu-бэкенда;
# darwin-vz собирается штатным swiftc из Xcode CLT
make build   # на macOS также собирает и подписывает bin/boxlet-vm
export PATH="$PWD/bin:$PATH"
```

### 2. Запуск

```sh
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

### 3. Опционально

```sh
boxlet priv install      # порты <1024 и симлинк /var/run/docker.sock
                         # (один раз, GUI-запрос пароля администратора)
boxlet dns install       # имена *.boxlet.local → адрес гостевой ОС
boxlet service install   # автозапуск демона при входе в систему
boxlet tray              # иконка в менюбаре: статус, метрики, start/stop
```

### Удаление

```sh
boxlet stop
boxlet service uninstall  # если включали автозапуск
boxlet dns uninstall      # если включали DNS
sudo rm /var/run/docker.sock  # если создавали симлинк
rm -rf ~/.boxlet /usr/local/bin/boxlet
```

## Структура репозитория

- `cmd/boxlet` — CLI (init/start/stop/status/doctor/shell/logs/dns/tray/set/service/upgrade/ps/...)
- `cmd/boxletd` — демон: управляет VM, сокет-прокси, DNS и веб-интерфейсом
- `cmd/boxlet-agent` — агент в гостевой ОС для `boxlet shell` (linux, static)
- `cmd/boxlet-priv` — setuid-хелпер: порты <1024, symlink /var/run/docker.sock,
  /etc/resolver/boxlet
- `internal/backend` — интерфейс драйвера + реализации per-платформа
- `internal/guest` — гостевая ОС: скачивание Alpine, apkovl-оверлей
- `internal/proxy` — проксирование Docker API сокета из VM на хост
- `internal/dnsd` — DNS-сервер зоны *.boxlet.local → адрес гостевой ОС
- `internal/config`, `internal/paths` — конфигурация и пути инстанса
- `internal/webui` — встроенный веб-интерфейс (статус, контейнеры,
  live-метрики, логи, рестарт)
- `vmhelper/` — Swift-хелпер `boxlet-vm` (Virtualization.framework)
  и entitlements для ad-hoc подписи
- `tray/` — менюбар-трей macOS `boxlet-tray` (AppKit, без зависимостей)
- `install.sh` — установщик из релиза (curl | bash)
- `.github/workflows/` — CI (vet, тесты, сборка) и релиз-пайплайн по тегу
- `docs/` — архитектура, роадмап, заметки по совместимости

## Документация

- [Архитектура](docs/architecture.md)
- [Совместимость](docs/compat.md)
- [Роадмап](docs/roadmap.md)

## Лицензия

[Apache License 2.0](LICENSE). Использование и производные работы требуют
сохранения уведомления из файла [NOTICE](NOTICE) со ссылкой на
исходный репозиторий (п. 4(d) лицензии).
