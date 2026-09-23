#!/bin/sh
# Installs the latest arranger release:
#   curl -fsSL https://raw.githubusercontent.com/arranger-dev/arranger/main/install.sh | sh
# Set ARRANGER_VERSION=v0.3.0 to pin a version, INSTALL_DIR to choose where it goes.
set -eu
repo=arranger-dev/arranger
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
case $os in darwin | linux) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac

file=arranger_${os}_${arch}.tar.gz
if [ -n "${ARRANGER_VERSION:-}" ]; then
  url=https://github.com/$repo/releases/download/$ARRANGER_VERSION
else
  url=https://github.com/$repo/releases/latest/download
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$url/$file" -o "$tmp/$file"
curl -fsSL "$url/checksums.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && grep " $file\$" checksums.txt | { sha256sum -c - 2>/dev/null || shasum -a 256 -c -; }) >/dev/null ||
  { echo "checksum mismatch for $file" >&2; exit 1; }
tar xzf "$tmp/$file" -C "$tmp"

dir=${INSTALL_DIR:-/usr/local/bin}
if [ -w "$dir" ]; then
  mv "$tmp/arranger" "$dir/arranger"
else
  echo "installing to $dir (needs sudo)"
  sudo mv "$tmp/arranger" "$dir/arranger"
fi
echo "installed $dir/arranger. Run: arranger  (then open http://127.0.0.1:7777)"
