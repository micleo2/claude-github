#!/usr/bin/env bash
# Build tether and run the end-to-end suite. Needs root, Docker, kernel >= 6.14.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
export TETHER_BIN_DIR=${TETHER_BIN_DIR:-/tmp/tether-bin}
mkdir -p "$TETHER_BIN_DIR"
# The daemon is built exactly as users build it (Syncthing's build.go). The
# helpers below are static so they run in any container.
(cd "$HERE/../tether" && go run build.go build >/dev/null && cp syncthing "$TETHER_BIN_DIR/syncthing" && go build -o "$TETHER_BIN_DIR/tether" ./cmd/tether)
(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$TETHER_BIN_DIR/probe" .)
(cd "$HERE/latproxy" && CGO_ENABLED=0 go build -o "$TETHER_BIN_DIR/latproxy" .)
# Walkers the prefetch tests measure (rg, git), taken from the host with the
# libraries the container lacks; glibc comes from the container.
mkdir -p "$TETHER_BIN_DIR/tools/lib"
for t in rg git; do
  if p=$(command -v $t); then
    cp "$p" "$TETHER_BIN_DIR/tools/"
    ldd "$p" | awk '$3 ~ /^\// {print $3}' | grep -v -e libc.so -e libm.so | xargs -r cp -L -t "$TETHER_BIN_DIR/tools/lib/"
  fi
done
exec python3 "$HERE/tether_e2e.py" "$@"
