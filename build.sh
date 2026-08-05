#!/bin/bash
# Build srt-relay-dashboard for Linux or macOS.
#
# Requires: Go, a C compiler, and libsrt 1.5.6+ (headers + lib).
#
# Linux (Debian/Ubuntu):
#   sudo apt install -y cmake build-essential pkg-config libssl-dev libzstd-dev
#   # then run ./build.sh linux
#
# macOS:
#   brew install srt  # or build libsrt from source
#   # then run ./build.sh darwin
#
# Windows: currently NOT recommended — srtgo on Windows crashes with a
# cgo/CRT exception (0x406d1388) in srt_startup. Use Linux or macOS.

set -euo pipefail

TARGET="${1:-linux}"

case "$TARGET" in
  linux)   GOOS=linux; GOARCH=amd64 ;;
  darwin)  GOOS=darwin; GOARCH=arm64 ;;
  *)       echo "usage: $0 [linux|darwin]"; exit 1 ;;
esac

echo "==> building srt-relay-dashboard for $GOOS/$GOARCH"

export CGO_ENABLED=1
export GOOS GOARCH

# libsrt discovery (pkg-config or standard locations)
SRT_CFLAGS="$(pkg-config --cflags srt 2>/dev/null || echo -I/usr/local/include)"
SRT_LIBS="$(pkg-config --libs srt 2>/dev/null || echo -L/usr/local/lib -lsrt)"

echo "    CFLAGS: $SRT_CFLAGS"
echo "    LIBS:   $SRT_LIBS"

go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "srt-relay-dashboard-${GOOS}-${GOARCH}" \
  .

echo "==> done: srt-relay-dashboard-${GOOS}-${GOARCH}"
echo "    On Linux you also need the matching libsrt shared lib at runtime:"
echo "      ldd ./srt-relay-dashboard-${GOOS}-${GOARCH}"
