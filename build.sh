#!/usr/bin/env bash
# Build the Miora CLIProxyAPI plugin on Linux / macOS.
#
# The plugin ABI is a C ABI, so CGO and a C toolchain are required. Cross builds
# need the matching compiler, e.g. for Windows from Linux:
#   CC=x86_64-w64-mingw32-gcc GOOS=windows bash build.sh
set -euo pipefail

cd "$(dirname "$0")"

GOOS_TARGET="${GOOS:-$(go env GOOS)}"
GOARCH_TARGET="${GOARCH:-$(go env GOARCH)}"

case "$GOOS_TARGET" in
  windows) EXT="dll" ;;
  darwin)  EXT="dylib" ;;
  *)       EXT="so" ;;
esac

OUT_DIR="plugins/${GOOS_TARGET}/${GOARCH_TARGET}"
mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/miora-provider.$EXT"

echo "building plugin -> $OUT"
CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o "$OUT" .

echo
echo "done. Verify the ABI exports with:"
case "$GOOS_TARGET" in
  windows) echo "  x86_64-w64-mingw32-objdump -p $OUT | grep cliproxy" ;;
  darwin)  echo "  nm -gU $OUT | grep cliproxy" ;;
  *)       echo "  nm -D --defined-only $OUT | grep cliproxy" ;;
esac
