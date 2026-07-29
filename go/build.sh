#!/bin/sh
# Cross-compile codex-meter for every target install.sh knows how to fetch.
#
#   sh build.sh [OUTDIR]               (default: ./dist)
#
# Binaries are not committed — this is what regenerates them.
#
# -trimpath keeps absolute build paths out of the binary, which matters for
# reproducibility: two people building the same commit should get the same
# bytes, and that is the only thing that makes "verify it against the source"
# a meaningful offer rather than a slogan.
set -eu

cd "$(dirname "$0")"
out="${1:-dist}"
mkdir -p "$out"

go="${GO:-go}"
command -v "$go" >/dev/null 2>&1 || go="$HOME/.local/go/bin/go"
command -v "$go" >/dev/null 2>&1 || { echo "no go toolchain found" >&2; exit 1; }

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os=${target%/*}
    arch=${target#*/}
    bin="$out/codex-meter-$os-$arch"
    # CGO off so the Linux builds are genuinely static — a helper that needs a
    # matching glibc is a helper that fails on someone's older VPS.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      "$go" build -trimpath -ldflags="-s -w" -o "$bin" .
    printf '%-24s %6s KiB\n' "$target" "$(( $(wc -c < "$bin") / 1024 ))"
done

# Checksums, so install.sh can grow a verification step and so a published
# release has something to compare against.
( cd "$out" && sha256sum codex-meter-* > SHA256SUMS 2>/dev/null \
  || shasum -a 256 codex-meter-* > SHA256SUMS )
echo "wrote $out/SHA256SUMS"
