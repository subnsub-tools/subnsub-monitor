#!/bin/sh
# Build, stamp the installer with the resulting checksums, and upload.
#
#   sh helper/go/release.sh            build + stamp only
#   sh helper/go/release.sh --upload   …and push everything to R2
#
# The stamping step is the point. install.sh carries the expected SHA-256 of
# each binary inline, so the file a user is invited to read before piping it
# into a shell is also the file that decides which bytes are acceptable. A
# checksum fetched at install time from the same host as the binary would
# authenticate nothing.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
dist="$here/dist"
installer="$repo/helper/install.sh"
psinstaller="$repo/helper/install.ps1"

# The published set, in one place, because five files have to agree about it:
# build.sh builds them, this script stamps and uploads them, the two installers
# carry a checksum for each, and functions/monitor/[asset].js decides which
# names the site will serve at all. A target missing from any one of those is a
# machine that downloads a 404 or refuses to install a binary it cannot verify.
#
# `_` here and `-` in the filename, because the shell variables the installers
# use cannot hold a dash.
#
# Windows is deliberately absent — see build.sh. install.ps1 is not stamped and
# not uploaded while that is true, and its checksums sit at PLACEHOLDER so a
# copy that escapes anyway cannot install anything.
TARGETS="linux_amd64 linux_arm64 linux_arm darwin_amd64 darwin_arm64 freebsd_amd64 freebsd_arm64 android_arm64"

# The published name for one target: dashes, and .exe on Windows. This is the
# name the HELPER builds for itself when it updates, out of runtime.GOOS and
# runtime.GOARCH, so it is a protocol rather than a convention.
asset_for() {
    case "$1" in
      windows_*) echo "subnsub-monitor-$(echo "$1" | tr '_' '-').exe" ;;
      *)         echo "subnsub-monitor-$(echo "$1" | tr '_' '-')" ;;
    esac
}

# RELEASE_PREBUILT=1 skips the two builds and takes what is already in the two
# dist directories. The build is eight cross-compiles and a wasm module, and
# the machine that holds the upload credentials is a loaded production host
# (2026-09-28: ten minutes unfinished there, load past 20). So the builds run
# on the build machine — same two scripts, same flags, which is what the
# reproducibility note in build.sh is for — the dist directories are carried
# back, and only the checks, the stamping and the upload happen here. Every
# check below still runs against the artefacts: the built binary is asked its
# version, and a missing target fails the stamping loop.
webdist="$repo/helper/tailcat-web/dist"
if [ "${RELEASE_PREBUILT:-}" = 1 ]; then
    [ -f "$dist/SHA256SUMS" ] && [ -f "$webdist/tailcat-web.wasm" ] || {
        echo "RELEASE_PREBUILT=1 but $dist or $webdist holds no build" >&2; exit 1; }
    ( cd "$dist" && sha256sum -c SHA256SUMS >/dev/null ) || {
        echo "$dist does not match its own SHA256SUMS" >&2; exit 1; }
    echo "prebuilt:        taking $dist and $webdist as they are"
else
    sh "$here/build.sh" "$dist"
    # The browser half of direct mode, built beside the binaries and published
    # with them: the page loads it by name and checks its SHA-256 against the
    # one stamped below, so the two must come from the same release.
    sh "$repo/helper/tailcat-web/build.sh" "$webdist"
fi

# What this release calls itself, taken from the one place that decides it.
#
# This value is what every machine with the update switch on compares itself
# against, so publishing one that does not match the binaries would either
# strand a fleet on an old build (too low) or send it into a loop of
# downloading something that keeps reporting a different version (too high).
# Both are checked below rather than trusted.
VERSION=$(sed -n 's/^const helperVersion = "\(.*\)"$/\1/p' "$here/version.go")
# YYYY.MM.DD.N exactly — the SAME shape the relay enforces (HELPER_VERSION_RE
# in monitor-worker/src/clean.js) and the helper's own validVersion. A looser
# check here is not untidy but harmful: "2999..1" is digits and dots, would
# publish, would install, and would then be refused by the relay when the new
# helper reported it — leaving a machine that runs perfectly, cannot name
# itself, and shows on the board as older than everything.
case "$VERSION" in
  [0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].[0-9]) ;;
  [0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].[0-9][0-9]) ;;
  [0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9].[0-9][0-9][0-9]) ;;
  *) echo "version.go must hold YYYY.MM.DD.N; got '$VERSION'" >&2; exit 1 ;;
esac

# The relay has to agree about what "current" is, and this is the only moment
# anyone is in a position to notice that it does not.
#
# The two constants are deployed by different commands on different schedules,
# so they drift silently: publish binaries the relay still calls old and every
# machine is told it needs an update it already has; leave the relay ahead and
# every machine is told to fetch a version that is not there. Checked here
# because here is where the binaries are about to be published.
# Everything this script publishes has to be something the site will SERVE.
#
# The download route is an allow-list of filenames (functions/monitor/[asset].js)
# and that list is the entire access model — an object uploaded under a name it
# does not carry sits in the bucket, reachable by nothing, and the only symptom
# is the feature that needed it failing at the far end on somebody else's
# machine. Caught the first time by VERSION, which uploaded cleanly and 404'd.
route="$repo/functions/monitor/[asset].js"
if [ -f "$route" ]; then
    names="VERSION SHA256SUMS install.sh"
    for t in $TARGETS; do names="$names $(asset_for "$t")"; done
    for n in $names; do
        grep -q "'$n':" "$route" || {
            echo "$n is published here but not served: add it to $route" >&2; exit 1; }
    done
fi

worker="$repo/monitor-worker/src/index.js"
if [ -f "$worker" ]; then
    advertised=$(sed -n "s/^const HELPER_CURRENT = '\(.*\)';.*/\1/p" "$worker")
    [ "$advertised" = "$VERSION" ] || {
        echo "monitor-worker advertises HELPER_CURRENT='$advertised' but this release is '$VERSION'" >&2
        echo "bump it in $worker, and remember to deploy the worker AFTER this upload" >&2
        exit 1; }
fi

# And ask a binary we just built whether it agrees. The parse above is a
# regular expression against a source file; this is the artefact itself, which
# is the thing users will actually run.
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)   native="$dist/subnsub-monitor-linux-amd64" ;;
  Linux/aarch64)  native="$dist/subnsub-monitor-linux-arm64" ;;
  Darwin/x86_64)  native="$dist/subnsub-monitor-darwin-amd64" ;;
  Darwin/arm64)   native="$dist/subnsub-monitor-darwin-arm64" ;;
  *)              native="" ;;
esac
if [ -n "$native" ] && [ -x "$native" ]; then
    said=$("$native" version 2>/dev/null || true)
    [ "$said" = "$VERSION" ] || {
        echo "version.go says '$VERSION' but the built binary says '$said'" >&2; exit 1; }
    echo "version:         $VERSION (confirmed by the built binary)"
else
    # Cross-building for a host we cannot execute on. Worth saying out loud —
    # the check that did not run is the one that catches a stale constant.
    echo "version:         $VERSION (unverified: no runnable build for this host)"
fi
printf '%s\n' "$VERSION" > "$dist/VERSION"

sum_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum < "$1" | cut -d' ' -f1
    else
        shasum -a 256 < "$1" | cut -d' ' -f1
    fi
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM
cp "$installer" "$tmp"
for target in $TARGETS; do
    bin="$dist/$(asset_for "$target")"
    [ -f "$bin" ] || { echo "missing $bin" >&2; exit 1; }
    s=$(sum_of "$bin")
    # Replace whatever the line currently holds — a placeholder on a fresh
    # checkout, the previous release's hash afterwards. Two files with two
    # syntaxes for the same fact; each one only carries the targets it can
    # install, and the placeholder check below is what catches a miss.
    sed -i.bak "s|^SUM_$target=.*|SUM_$target=$s|" "$tmp" && rm -f "$tmp.bak"
    printf '  %-16s %s\n' "$target" "$s"
done
mv "$tmp" "$installer"
trap - EXIT INT TERM
chmod +x "$installer"
echo "stamped $installer"

# And the page: monitor-direct.js carries the wasm module's SHA-256 and the
# version it belongs to, for the same reason install.sh carries the binaries'
# — the file that decides which bytes are acceptable is one somebody can
# read, and it is not the host the bytes come from. The version doubles as
# the cache-buster on the fetch URL.
direct="$repo/monitor-direct.js"
wsum=$(cat "$webdist/tailcat-web.sha256")
esum=$(cat "$webdist/tailcat-web-exec.sha256")
for x in "$wsum" "$esum"; do
    case "$x" in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
      *) echo "no checksum in $webdist" >&2; exit 1 ;;
    esac
done
tmp2=$(mktemp)
sed -e "s|^  const WASM_V = '.*';|  const WASM_V = '$VERSION';|" \
    -e "s|^  const WASM_SHA256 = '.*';|  const WASM_SHA256 = '$wsum';|" \
    -e "s|^  const EXEC_SHA256 = '.*';|  const EXEC_SHA256 = '$esum';|" "$direct" > "$tmp2"
grep -q "WASM_SHA256 = '$wsum'" "$tmp2" && grep -q "EXEC_SHA256 = '$esum'" "$tmp2" \
  || { echo "could not stamp $direct" >&2; rm -f "$tmp2"; exit 1; }
mv "$tmp2" "$direct"
echo "stamped $direct (wasm $wsum)"

# Refuse to ship an installer that would refuse to install.
if grep -q '^SUM_.*=PLACEHOLDER' "$installer"; then
    echo "installer still has placeholder checksums" >&2
    exit 1
fi
# And the reverse for the one that is NOT being shipped: install.ps1 must still
# be holding placeholders, because a stale REAL checksum in an unpublished
# installer is the trap whoever re-enables Windows would walk into — it would
# look stamped, and it would name binaries from whichever release happened to be
# current when the plug was pulled.
if ! grep -q "PLACEHOLDER_windows" "$psinstaller"; then
    echo "install.ps1 is not published but carries real checksums" >&2
    echo "reset its SUM_ lines to PLACEHOLDER_*, or put windows back in TARGETS" >&2
    exit 1
fi

[ "${1:-}" = "--upload" ] || { echo "(dry run — pass --upload to publish)"; exit 0; }

cd "$repo"
set -a; . "$repo/.env.cloudflare"; set +a
# By NAME from $TARGETS, never by globbing $dist. The directory is not cleaned
# between releases by anything but build.sh, so a glob would happily re-upload
# an artefact for a target that has since been dropped — which is exactly how a
# withdrawn platform quietly comes back.
for t in $TARGETS; do
    n=$(asset_for "$t")
    npx wrangler r2 object put "link/monitor/$n" --file "$dist/$n" \
        --content-type application/octet-stream --remote >/dev/null
    echo "  uploaded $n"
done
npx wrangler r2 object put "link/monitor/SHA256SUMS" --file "$dist/SHA256SUMS" \
    --content-type text/plain --remote >/dev/null
# The wasm and its loader go up BEFORE the installer and VERSION for the same
# reason the binaries do: the page that names this checksum is deployed with
# the site, and until the object is there that page loads nothing.
# Versioned names (functions/monitor/[asset].js VERSIONED): each release's
# pair is a new object, and earlier pairs are never overwritten, so pages
# and rollbacks keep finding the module their digest names.
npx wrangler r2 object put "link/monitor/tailcat-web-$VERSION.wasm" --file "$webdist/tailcat-web.wasm" \
    --content-type application/wasm --remote >/dev/null
npx wrangler r2 object put "link/monitor/tailcat-web-$VERSION-exec.js" --file "$webdist/tailcat-web-exec.js" \
    --content-type text/javascript --remote >/dev/null
echo "  uploaded tailcat-web-$VERSION.wasm + tailcat-web-$VERSION-exec.js"
# The installers go up LAST: until they do, the published ones still point at
# the previous binaries, and they should never name hashes that are not yet
# fetchable.
npx wrangler r2 object put "link/monitor/install.sh" --file "$installer" \
    --content-type text/x-shellscript --remote >/dev/null
echo "  uploaded install.sh (stamped)"
# VERSION goes up LAST OF ALL, after even the installer. It is the trigger: a
# machine with the update switch on reads this file, and every earlier object
# has to be fetchable before it starts pointing at them. Published first, it
# would send the fleet at a 404 — and each of those machines would report a
# failed update for a release that was fine.
npx wrangler r2 object put "link/monitor/VERSION" --file "$dist/VERSION" \
    --content-type text/plain --remote >/dev/null
echo "  uploaded VERSION ($VERSION)"
