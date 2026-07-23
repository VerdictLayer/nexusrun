#!/usr/bin/env bash
# Cross-compile nexus for every supported platform.
#
# Everything is pure Go with CGO disabled, so each binary is a single
# static file with no runtime dependencies — it drops onto a Raspberry Pi,
# a locked-down server, or a fresh laptop and just runs.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
OUT="${OUT:-dist}"

LDFLAGS="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT}"

PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"       # Raspberry Pi 2/3/4 running 32-bit Raspberry Pi OS
  "darwin/amd64"    # Intel Mac
  "darwin/arm64"    # Apple Silicon
  "windows/amd64"
  "windows/arm64"   # Snapdragon X / Copilot+ PCs
  "freebsd/amd64"
)

rm -rf "$OUT"
mkdir -p "$OUT"

echo "Building nexus ${VERSION} (${COMMIT})"
for platform in "${PLATFORMS[@]}"; do
  GOOS="${platform%%/*}"
  GOARCH="${platform##*/}"

  name="nexus-${GOOS}-${GOARCH}"
  bin="nexus"
  [ "$GOOS" = "windows" ] && bin="nexus.exe"

  workdir="${OUT}/${name}"
  mkdir -p "$workdir"

  extra=()
  [ "$GOARCH" = "arm" ] && extra+=("GOARM=7")

  env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" "${extra[@]}" \
    go build -trimpath -ldflags "$LDFLAGS" -o "${workdir}/${bin}" ./cmd/nexus

  cp README.md LICENSE "$workdir/" 2>/dev/null || true

  if [ "$GOOS" = "windows" ]; then
    (cd "$OUT" && zip -qr "${name}.zip" "$name")
  else
    tar -czf "${OUT}/${name}.tar.gz" -C "$OUT" "$name"
  fi
  rm -rf "$workdir"

  size=$(du -h "${OUT}/${name}".* | head -1 | cut -f1)
  printf "  %-26s %s\n" "$name" "$size"
done

# Checksums let users verify downloads without a package manager.
(cd "$OUT" && sha256sum ./*.tar.gz ./*.zip 2>/dev/null > checksums.txt || true)

echo
echo "Artifacts in ${OUT}/"
