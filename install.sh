#!/bin/sh
# Установщик boxlet (macOS, Apple Silicon):
#
#   curl -fsSL https://raw.githubusercontent.com/alexandr-mironov/boxlet/main/install.sh | bash
#
# Что делает: скачивает последний релиз с GitHub, проверяет sha256,
# распаковывает бинари в ~/.boxlet/bin (они зависят друг от друга и
# должны лежать вместе) и, если есть права, создаёт симлинк
# /usr/local/bin/boxlet. Ничего системного не трогает, sudo не нужен.
set -eu

REPO="alexandr-mironov/boxlet"
INSTALL_DIR="$HOME/.boxlet/bin"
LINK_DIR="/usr/local/bin"

if [ "$(uname -s)" != "Darwin" ]; then
    echo "boxlet: пока поддерживается только macOS" >&2
    exit 1
fi
if [ "$(uname -m)" != "arm64" ]; then
    echo "boxlet: пока поддерживается только Apple Silicon (arm64)" >&2
    exit 1
fi

echo "==> ищу последний релиз $REPO"
URL=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep browser_download_url | grep 'darwin-arm64\.tar\.gz"' | cut -d'"' -f4)
if [ -z "$URL" ]; then
    echo "boxlet: релиз darwin-arm64 не найден" >&2
    exit 1
fi
FILE=$(basename "$URL")
VER=$(echo "$FILE" | sed 's/^boxlet_//; s/_darwin-arm64\.tar\.gz$//')

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "==> скачиваю boxlet $VER"
curl -fsSL "$URL" -o "$TMP/$FILE"
curl -fsSL "$URL.sha256" -o "$TMP/$FILE.sha256"

echo "==> проверяю контрольную сумму"
( cd "$TMP" && shasum -a 256 -c "$FILE.sha256" >/dev/null )

echo "==> устанавливаю в $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
tar -xzf "$TMP/$FILE" -C "$TMP"
PKG="${FILE%.tar.gz}"
# shellcheck disable=SC2086
cp -f "$TMP/$PKG"/boxlet* "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR"/boxlet*

if [ -d "$LINK_DIR" ] && [ -w "$LINK_DIR" ]; then
    ln -sf "$INSTALL_DIR/boxlet" "$LINK_DIR/boxlet"
    echo "==> $LINK_DIR/boxlet → $INSTALL_DIR/boxlet"
else
    echo "==> добавьте в PATH: export PATH=\"$INSTALL_DIR:\$PATH\""
fi

echo
echo "Готово: $("$INSTALL_DIR/boxlet" version)"
echo "Дальше: boxlet init && boxlet start"
