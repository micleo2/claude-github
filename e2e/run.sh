#!/usr/bin/env bash
# Build tether and run the end-to-end suite. Needs root, Docker, kernel >= 6.14.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
export TETHER_BIN_DIR=${TETHER_BIN_DIR:-/tmp/tether-bin}
mkdir -p "$TETHER_BIN_DIR"
(cd "$HERE/../tether" && go run build.go assets >/dev/null && CGO_ENABLED=0 go build -o "$TETHER_BIN_DIR/syncthing" ./cmd/syncthing && CGO_ENABLED=0 go build -o "$TETHER_BIN_DIR/tether" ./cmd/tether)
(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$TETHER_BIN_DIR/probe" .)
exec python3 "$HERE/tether_e2e.py" "$@"
