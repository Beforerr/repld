#!/usr/bin/env bash
set -euo pipefail

REPO="Beforerr/repld"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Linux)  os="linux" ;;
  Darwin) os="darwin" ;;
  *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

case "$ARCH" in
  x86_64)          arch="amd64" ;;
  arm64|aarch64)   arch="arm64" ;;
  *)               echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

release_url="https://api.github.com/repos/${REPO}/releases/latest"
if [[ -n "${VERSION:-}" ]]; then
  release_url="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
fi

release_json="$(curl -fsSL "$release_url")"
if [[ -z "${VERSION:-}" ]]; then
  VERSION="$(printf '%s\n' "$release_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
fi

asset_suffix="_${os}_${arch}.tar.gz"
ASSET="$(printf '%s\n' "$release_json" | sed -n "s/.*\"name\": *\"\\([^\"]*${asset_suffix}\\)\".*/\\1/p" | head -1)"
if [[ -z "$ASSET" ]]; then
  echo "No ${os}/${arch} tar.gz asset found for ${REPO} ${VERSION}" >&2
  exit 1
fi

asset_name="${ASSET%$asset_suffix}"
install_name="${BIN_NAME:-$asset_name}"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

echo "Installing ${install_name} ${VERSION} (${os}/${arch}) -> ${INSTALL_DIR}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "$TMP/$ASSET"
tar -xzf "$TMP/$ASSET" -C "$TMP"
mkdir -p "$INSTALL_DIR"
src="$TMP/$asset_name"
if [[ ! -f "$src" ]]; then
  src="$(find "$TMP" -type f -perm -111 | head -1)"
fi
if [[ -z "$src" || ! -f "$src" ]]; then
  echo "No executable found in ${ASSET}" >&2
  exit 1
fi
mv "$src" "$INSTALL_DIR/$install_name"
chmod +x "$INSTALL_DIR/$install_name"

echo "Done. Make sure ${INSTALL_DIR} is on your \$PATH."
