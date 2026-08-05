#!/bin/sh
# Stamp a dist/install.sh whose inline checksums are THESE binaries'.
#
# The repository's install.sh carries the sums of the last published release,
# and the installer verifies against its inline SUM_* lines only — it never
# reads the SHA256SUMS file beside a binary, because a checksum fetched from
# the same place as the binary proves nothing. So an installer copied as-is
# next to binaries built from any later source fails its own verification, on
# purpose. This writes the copy a `-dist` directory should serve: the same
# installer, sums replaced with the ones build.sh just wrote, plus the VERSION
# the helper source says it is.
#
# Everything is built in temp files and published with mv — one rename each —
# because the directory this writes into may be BEING SERVED: a relay running
# -dist against it must never hand out a half-stamped installer, and a
# symlink or FIFO squatting on the destination name must be replaced as a
# name, never opened or written through.
#
#   cd go && sh build.sh && sh stamp.sh    # → dist/install.sh, dist/VERSION
set -eu
cd "$(dirname "$0")"
[ -f dist/SHA256SUMS ] || { echo "stamp.sh: no dist/SHA256SUMS — run build.sh first" >&2; exit 1; }

tmp=
prog=
cleanup() { rm -f ${tmp:+"$tmp"} ${prog:+"$prog"}; }
trap cleanup EXIT INT TERM

# One sed program built from the manifest, applied in one pass. POSIX sed —
# no -i, whose spelling GNU and BSD cannot agree on.
prog=$(mktemp dist/.stamp.XXXXXX)
while read -r sum name; do
    case "$name" in
    subnsub-monitor-*)
        key=$(printf '%s' "${name#subnsub-monitor-}" | tr '-' '_')
        # Every built binary must have a SUM_ line to land in. A target the
        # installer has never heard of would sit in the directory verifying
        # never — refusing here surfaces the mismatch at build time instead
        # of on somebody else's machine.
        grep -q "^SUM_${key}=" ../install.sh || {
            echo "stamp.sh: install.sh has no SUM_${key} line for $name" >&2
            exit 1
        }
        printf 's|^SUM_%s=.*|SUM_%s=%s|\n' "$key" "$key" "$sum" >> "$prog"
        ;;
    esac
done < dist/SHA256SUMS

tmp=$(mktemp dist/.stamp.XXXXXX)
sed -f "$prog" ../install.sh > "$tmp"

# Believe the result, not the intent: every manifest line must now appear
# verbatim, or nothing is published.
while read -r sum name; do
    case "$name" in
    subnsub-monitor-*)
        key=$(printf '%s' "${name#subnsub-monitor-}" | tr '-' '_')
        grep -q "^SUM_${key}=${sum}\$" "$tmp" || {
            echo "stamp.sh: SUM_${key} did not take" >&2
            exit 1
        }
        ;;
    esac
done < dist/SHA256SUMS

chmod 644 "$tmp"
mv "$tmp" dist/install.sh
tmp=

ver=$(sed -n 's/^const helperVersion = "\(.*\)"$/\1/p' version.go)
[ -n "$ver" ] || { echo "stamp.sh: cannot read helperVersion from version.go" >&2; exit 1; }
tmp=$(mktemp dist/.stamp.XXXXXX)
printf '%s\n' "$ver" > "$tmp"
chmod 644 "$tmp"
mv "$tmp" dist/VERSION
tmp=

echo "stamped dist/install.sh (${ver}) — its sums are the binaries beside it"
