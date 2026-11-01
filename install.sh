#!/bin/sh
# Install kdeck on Linux or macOS, checksum-verified.
#   curl -fsSL https://raw.githubusercontent.com/lyelgate/kdeck/main/install.sh | sh
# Options (environment): KDECK_VERSION=v0.1.0  KDECK_INSTALL_DIR=/usr/local/bin  KDECK_REPO=owner/name
set -eu
REPO=${KDECK_REPO:-lyelgate/kdeck}
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in linux|darwin) ;; *) echo "unsupported OS $OS (Windows: use install.ps1)"; exit 1;; esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported CPU $(uname -m)"; exit 1 ;;
esac
VERSION=${KDECK_VERSION:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)}
[ -n "$VERSION" ] || { echo "could not find the latest release of $REPO"; exit 1; }
FILE="kdeck_${VERSION#v}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
echo "Downloading $FILE ($VERSION)"
curl -fsSL -o "$TMP/$FILE" "$BASE/$FILE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"
WANT=$(grep " $FILE\$" "$TMP/checksums.txt" | awk '{print $1}')
if command -v sha256sum >/dev/null 2>&1; then GOT=$(sha256sum "$TMP/$FILE" | awk '{print $1}'); else GOT=$(shasum -a 256 "$TMP/$FILE" | awk '{print $1}'); fi
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "checksum mismatch for $FILE; not installing"; exit 1; }
tar -xzf "$TMP/$FILE" -C "$TMP" kdeck
DIR=${KDECK_INSTALL_DIR:-/usr/local/bin}
if [ -w "$DIR" ]; then install -m 0755 "$TMP/kdeck" "$DIR/kdeck"
elif [ -z "${KDECK_INSTALL_DIR:-}" ] && command -v sudo >/dev/null 2>&1 && [ -t 0 ]; then sudo install -m 0755 "$TMP/kdeck" "$DIR/kdeck"
else DIR="$HOME/.local/bin"; mkdir -p "$DIR"; install -m 0755 "$TMP/kdeck" "$DIR/kdeck"; echo "Installed to $DIR; make sure it is on PATH."
fi
echo "Installed $("$DIR/kdeck" about | head -1) at $DIR/kdeck"
echo "Next: kdeck setup"
