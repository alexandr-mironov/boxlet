#!/bin/sh
# Установка и обновление boxlet (macOS, Apple Silicon) одной командой:
#
#   curl -fsSL https://raw.githubusercontent.com/alexandr-mironov/boxlet/main/install.sh | bash
#
# Что делает:
#   1. скачивает последний релиз с GitHub и проверяет sha256;
#   2. если boxletd этой установки работает — штатно останавливает его
#      (VM выключается со сбросом данных на диск);
#   3. кладёт бинари в ~/.boxlet/bin (они зависят друг от друга и должны
#      лежать вместе), подменяя каждый атомарно;
#   4. для новой установки — boxlet init (конфиг и каталоги);
#      существующий конфиг не трогается;
#   5. один раз спрашивает пароль администратора (sudo), и только если
#      это нужно: симлинк /usr/local/bin/boxlet и privileged helper для
#      портов <1024 (если он уже стоял или в конфиге есть такие порты);
#   6. запускает движок (или перезапускает автозапуск) и показывает
#      boxlet doctor.
#
# Повторный запуск — это обновление: те же шаги, пароль спрашивается,
# только если хелпер или симлинк нужно обновить.
#
# Переменные окружения:
#   BOXLET_NO_SUDO=1    не спрашивать пароль: без симлинка (PATH допишется
#                       в профиль shell) и без хелпера
#   BOXLET_NO_START=1   не запускать движок после установки
#   BOXLET_VERSION=v…   поставить конкретный релиз вместо последнего
#   BOXLET_TARBALL=…    поставить из локального архива (проверка сборки)
#   BOXLET_HOME=…       каталог инстанса (по умолчанию ~/.boxlet)
#   BOXLET_LINK_DIR=…   куда класть симлинк (по умолчанию /usr/local/bin)
set -eu

REPO="alexandr-mironov/boxlet"
BOXLET_DIR="${BOXLET_HOME:-$HOME/.boxlet}"
INSTALL_DIR="$BOXLET_DIR/bin"
LINK_DIR="${BOXLET_LINK_DIR:-/usr/local/bin}"
LINK="$LINK_DIR/boxlet"
LABEL="com.boxlet.boxletd"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BOXLET="$INSTALL_DIR/boxlet"

say() { printf '==> %s\n' "$*"; }
warn() { printf '!!  %s\n' "$*" >&2; }

if [ "$(uname -s)" != "Darwin" ]; then
    echo "boxlet: пока поддерживается только macOS" >&2
    exit 1
fi
if [ "$(uname -m)" != "arm64" ]; then
    echo "boxlet: пока поддерживается только Apple Silicon (arm64)" >&2
    exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --- 1. релиз -------------------------------------------------------------
if [ -n "${BOXLET_TARBALL:-}" ]; then
    FILE=$(basename "$BOXLET_TARBALL")
    say "ставлю из локального архива $BOXLET_TARBALL"
    cp "$BOXLET_TARBALL" "$TMP/$FILE"
    if [ -f "$BOXLET_TARBALL.sha256" ]; then
        cp "$BOXLET_TARBALL.sha256" "$TMP/$FILE.sha256"
    fi
else
    if [ -n "${BOXLET_VERSION:-}" ]; then
        API="https://api.github.com/repos/$REPO/releases/tags/$BOXLET_VERSION"
        say "ищу релиз $BOXLET_VERSION"
    else
        API="https://api.github.com/repos/$REPO/releases/latest"
        say "ищу последний релиз $REPO"
    fi
    URL=$(curl -fsSL "$API" | grep browser_download_url | grep 'darwin-arm64\.tar\.gz"' | cut -d'"' -f4)
    if [ -z "$URL" ]; then
        echo "boxlet: релиз darwin-arm64 не найден" >&2
        exit 1
    fi
    FILE=$(basename "$URL")
    say "скачиваю $FILE"
    curl -fsSL "$URL" -o "$TMP/$FILE"
    curl -fsSL "$URL.sha256" -o "$TMP/$FILE.sha256"
fi
if [ -f "$TMP/$FILE.sha256" ]; then
    say "проверяю контрольную сумму"
    ( cd "$TMP" && shasum -a 256 -c "$FILE.sha256" >/dev/null )
fi
tar -xzf "$TMP/$FILE" -C "$TMP"
PKG="$TMP/${FILE%.tar.gz}"

# --- 2. остановка работающего демона ---------------------------------------
# Только демона ЭТОЙ установки (по его pid-файлу), а не любой boxletd.
WAS_RUNNING=0
PIDFILE="$BOXLET_DIR/run/boxletd.pid"
if [ -f "$PIDFILE" ]; then
    PID=$(cat "$PIDFILE" 2>/dev/null || true)
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null &&
        ps -p "$PID" -o comm= 2>/dev/null | grep -q boxletd; then
        WAS_RUNNING=1
        say "останавливаю работающий boxletd (pid $PID), VM выключится штатно…"
        kill -TERM "$PID"
        i=0
        while kill -0 "$PID" 2>/dev/null && [ $i -lt 90 ]; do
            sleep 1
            i=$((i + 1))
        done
        if kill -0 "$PID" 2>/dev/null; then
            warn "boxletd не остановился за 90 с — завершаю принудительно"
            kill -KILL "$PID" 2>/dev/null || true
        fi
    fi
fi

# --- 3. бинари --------------------------------------------------------------
# Хелпер, установленный старыми версиями прямо в ~/.boxlet/bin (setuid root),
# сейчас будет заменён обычной копией — запоминаем, чтобы переустановить
# его в новое место.
HAD_LEGACY_PRIV=0
if [ -u "$INSTALL_DIR/boxlet-priv" ] && [ "$(stat -f %Su "$INSTALL_DIR/boxlet-priv")" = root ]; then
    HAD_LEGACY_PRIV=1
fi

say "устанавливаю в $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
for f in "$PKG"/boxlet*; do
    name=$(basename "$f")
    # Через временный файл и mv: работающий процесс со старым бинарём
    # не получит переписанный на ходу файл.
    cp "$f" "$INSTALL_DIR/.$name.new"
    chmod +x "$INSTALL_DIR/.$name.new"
    mv -f "$INSTALL_DIR/.$name.new" "$INSTALL_DIR/$name"
done

# --- 4. конфиг ---------------------------------------------------------------
# init только для новой установки: в релизах до v0.0.3 init перезаписывал
# существующий конфиг, а скрипт может поставить и такой релиз
# (BOXLET_VERSION или окно между слиянием и выпуском релиза).
if [ -f "$BOXLET_DIR/config.json" ]; then
    mkdir -p "$BOXLET_DIR/run" "$BOXLET_DIR/logs"
else
    "$BOXLET" init
fi

# --- 5. то, что требует пароля ---------------------------------------------
NEED_LINK=0
if [ "$(readlink "$LINK" 2>/dev/null || true)" != "$BOXLET" ]; then
    if [ -e "$LINK" ] && [ ! -L "$LINK" ]; then
        warn "$LINK — не симлинк boxlet, не трогаю"
    else
        NEED_LINK=1
    fi
fi

NEED_PRIV=0
if [ "$HAD_LEGACY_PRIV" = 1 ]; then
    NEED_PRIV=1
else
    rc=0
    "$BOXLET" priv status >/dev/null 2>&1 || rc=$?
    case $rc in
        0) ;;
        10) NEED_PRIV=1 ;;
        *) warn "не удалось проверить privileged helper (код $rc)" ;;
    esac
fi

# Симлинк без пароля, если каталог доступен на запись.
if [ "$NEED_LINK" = 1 ] && [ -d "$LINK_DIR" ] && [ -w "$LINK_DIR" ]; then
    ln -sfn "$BOXLET" "$LINK"
    say "$LINK → $BOXLET"
    NEED_LINK=0
fi

SUDO_OK=0
if [ "$NEED_LINK" = 1 ] || [ "$NEED_PRIV" = 1 ]; then
    if [ "${BOXLET_NO_SUDO:-}" = 1 ]; then
        say "BOXLET_NO_SUDO=1 — пропускаю шаги, требующие пароля"
    else
        WHY=""
        [ "$NEED_LINK" = 1 ] && WHY="симлинк $LINK"
        [ "$NEED_PRIV" = 1 ] && WHY="${WHY:+$WHY и }privileged helper для портов <1024"
        say "нужен пароль администратора (один раз): $WHY"
        # sudo читает пароль из терминала (/dev/tty), поэтому работает
        # и при запуске через curl | bash.
        if sudo -v </dev/tty; then
            SUDO_OK=1
        else
            warn "sudo недоступен или пароль не введён — продолжаю без этих шагов"
        fi
    fi
fi

if [ "$NEED_LINK" = 1 ] && [ "$SUDO_OK" = 1 ]; then
    sudo mkdir -p "$LINK_DIR"
    sudo ln -sfn "$BOXLET" "$LINK"
    say "$LINK → $BOXLET"
    NEED_LINK=0
fi
if [ "$NEED_PRIV" = 1 ]; then
    if [ "$SUDO_OK" = 1 ]; then
        "$BOXLET" priv install --sudo </dev/tty
    else
        warn "privileged helper не установлен или не обновлён: порты <1024 могут не пробрасываться (позже: boxlet priv install)"
    fi
fi

# Симлинка нет — дописываем PATH в профиль shell (один раз).
RC_FILE=""
if [ "$NEED_LINK" = 1 ]; then
    case "${SHELL:-}" in
        */zsh) RC_FILE="$HOME/.zshrc" ;;
        */bash) RC_FILE="$HOME/.bash_profile" ;;
        *) RC_FILE="$HOME/.profile" ;;
    esac
    LINE="export PATH=\"$INSTALL_DIR:\$PATH\""
    if ! grep -qsF "$LINE" "$RC_FILE"; then
        printf '\n# boxlet\n%s\n' "$LINE" >>"$RC_FILE"
        say "PATH дописан в $RC_FILE"
    fi
fi

# --- 6. запуск -------------------------------------------------------------
if [ "${BOXLET_NO_START:-}" = 1 ]; then
    say "BOXLET_NO_START=1 — движок не запускаю"
elif [ -f "$PLIST" ] && launchctl kickstart -k "gui/$(id -u)/$LABEL" 2>/dev/null; then
    say "автозапуск boxletd (launchd) перезапущен"
else
    if [ "$WAS_RUNNING" = 1 ]; then
        say "запускаю движок снова"
    else
        say "запускаю движок (первый запуск скачивает образы — несколько минут)"
    fi
    "$BOXLET" start || warn "движок не запустился — см. boxlet status и ~/.boxlet/logs"
fi

echo
"$BOXLET" doctor || true
echo
echo "Готово: $("$BOXLET" version)"
echo "Docker API: export DOCKER_HOST=unix://$BOXLET_DIR/run/docker.sock"
if [ -n "$RC_FILE" ]; then
    echo "Откройте новый терминал (или: source $RC_FILE), чтобы команда boxlet была в PATH."
fi
