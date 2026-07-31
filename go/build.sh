#!/bin/sh
# Cross-compile subnsub-monitor for every target install.sh knows how to fetch.
#
#   sh helper/go/build.sh [OUTDIR]     (default: helper/go/dist)
#
# Binaries are not committed — this is what regenerates them.
#
# -trimpath keeps absolute build paths out of the binary and -buildvcs=false
# keeps the git state out of it. Both are needed for reproducibility: two
# people building the same source should get the same bytes, and that is the
# only thing that makes "verify it against the source" a meaningful offer
# rather than a slogan.
#
# -buildvcs=false was NOT there at first, and the omission was invisible until
# measured: Go stamps the commit hash and dirty flag into every binary built
# inside a work tree, so the same source built from a checkout, a tarball, or
# our own tree produced three different binaries. Anyone following the README's
# invitation to verify would have found a mismatch and had no way to tell an
# innocent stamp from a swapped binary. Losing the embedded commit is the
# cheaper trade — SHA256SUMS already ties a release to its bytes.
set -eu

cd "$(dirname "$0")"
out="${1:-dist}"
mkdir -p "$out"
# Clear what a PREVIOUS run left, because this list shrinks as well as grows.
# Dropping a target otherwise leaves its binary sitting in the output directory,
# where the checksum manifest below still finds it and (until it stopped
# globbing) the upload step did too — a platform that was withdrawn republishing
# itself out of a stale file is the exact accident this line prevents.
rm -f "$out"/subnsub-monitor-* "$out/SHA256SUMS"

go="${GO:-go}"
command -v "$go" >/dev/null 2>&1 || go="$HOME/.local/go/bin/go"
command -v "$go" >/dev/null 2>&1 || { echo "no go toolchain found" >&2; exit 1; }

# Every target install.sh and install.ps1 know how to fetch.
#
# The NAMES matter as much as the list: a helper asked to update itself builds
# the asset name from its own runtime.GOOS and runtime.GOARCH, so
# `subnsub-monitor-<goos>-<goarch>` is not a convention here, it is the protocol.
# Windows carries .exe on top of that, because Windows will not execute a file
# without an extension it recognises and neither will Go's own exec resolution.
#
# linux/arm is built for ARMv7 explicitly. Left to the default the toolchain
# targets ARMv6, which runs on a Pi 1 and gives up the atomics and the barrier
# instructions every later board has. Naming the version is also the only way to
# be sure two people building this source get the same bytes.
#
# WINDOWS IS NOT IN THIS LIST, and the source for it still is. The port works
# and was published for a few hours on 2026-08-01; the decision to stop offering
# it is a product one, not a defect. Everything Windows-specific is behind a
# `//go:build windows` tag, so it cannot reach any binary built here, and
# `GOOS=windows go vet ./...` still passes — which is what keeps it from rotting
# while it is switched off. Re-enabling is this line, release.sh's TARGETS, and
# three names in functions/monitor/[asset].js.
for target in linux/amd64 linux/arm64 linux/arm \
              darwin/amd64 darwin/arm64 \
              freebsd/amd64 freebsd/arm64; do
    os=${target%/*}
    arch=${target#*/}
    ext=""
    [ "$os" = windows ] && ext=".exe"
    bin="$out/subnsub-monitor-$os-$arch$ext"
    goarm=""
    [ "$arch" = arm ] && goarm=7
    # CGO off so the Linux builds are genuinely static — a helper that needs a
    # matching glibc is a helper that fails on someone's older VPS. It is also
    # what keeps the Windows build free of a toolchain nobody cross-compiling
    # from Linux has.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$goarm" \
      "$go" build -trimpath -buildvcs=false -ldflags="-s -w" -o "$bin" .
    printf '%-24s %6s KiB\n' "$target" "$(( $(wc -c < "$bin") / 1024 ))"
done

# Checksums, so install.sh can grow a verification step and so a published
# release has something to compare against.
( cd "$out" && sha256sum subnsub-monitor-* > SHA256SUMS 2>/dev/null \
  || shasum -a 256 subnsub-monitor-* > SHA256SUMS )
echo "wrote $out/SHA256SUMS"
