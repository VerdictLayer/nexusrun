#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
  echo "usage: $0 LLAMA_CPP_CHECKOUT REVISION linux/amd64\|linux/arm64 LIBC_BASELINE OUTPUT" >&2
  exit 2
fi
src=$1
revision=$2
target=$3
libc=$4
output=$5

actual=$(git -C "$src" rev-parse HEAD)
expected=$(git -C "$src" rev-parse "$revision^{commit}")
test "$actual" = "$expected" || { echo "checkout is $actual, expected $expected" >&2; exit 1; }
test -z "$(git -C "$src" status --porcelain)" || { echo "llama.cpp checkout is dirty" >&2; exit 1; }

case "$target" in
  linux/amd64) arch_flags=-march=x86-64 ;;
  linux/arm64) arch_flags=-march=armv8-a ;;
  *) echo "unsupported first-release target: $target" >&2; exit 2 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cmake -S "$src" -B "$work/build" \
  -DCMAKE_BUILD_TYPE=Release \
  -DBUILD_SHARED_LIBS=OFF \
  -DGGML_NATIVE=OFF \
  -DGGML_CUDA=OFF -DGGML_VULKAN=OFF -DGGML_HIP=OFF \
  -DCMAKE_C_FLAGS="$arch_flags" -DCMAKE_CXX_FLAGS="$arch_flags"
cmake --build "$work/build" --config Release --target llama-cli llama-server --parallel
mkdir -p "$work/package/bin" "$work/package/licenses"
cp "$work/build/bin/llama-cli" "$work/package/bin/llama-cli"
cp "$work/build/bin/llama-server" "$work/package/bin/llama-server"
cp "$src/LICENSE" "$work/package/licenses/llama.cpp-LICENSE"

go run ./scripts/runtime-package \
  --source "$work/package" --output "$output" \
  --version "$revision" --revision "$actual" --target "$target" \
  --libc "$libc" --licenses MIT \
  --provenance "local reproducible build from https://github.com/ggml-org/llama.cpp@$actual"
