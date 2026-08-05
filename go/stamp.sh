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
#   cd go && sh build.sh && sh stamp.sh    # → dist/install.sh, dist/VERSION
set -eu
cd "$(dirname "$0")"
[ -f dist/SHA256SUMS ] || { echo "stamp.sh: no dist/SHA256SUMS — run build.sh first" >&2; exit 1; }

cp ../install.sh dist/install.sh
while read -r sum name; do
    case "$name" in
    subnsub-monitor-*)
        key=$(printf '%s' "${name#subnsub-monitor-}" | tr '-' '_')
        # Every built binary must have a SUM_ line to land in. A target the
        # installer has never heard of would sit in the directory verifying
        # never — refusing here is what surfaces the mismatch at build time
        # instead of on somebody else's machine.
        grep -q "^SUM_${key}=" dist/install.sh || {
            echo "stamp.sh: install.sh has no SUM_${key} line for $name" >&2
            exit 1
        }
        sed -i.bak "s/^SUM_${key}=.*/SUM_${key}=${sum}/" dist/install.sh
        rm -f dist/install.sh.bak
        ;;
    esac
done < dist/SHA256SUMS

ver=$(sed -n 's/^const helperVersion = "\(.*\)"$/\1/p' version.go)
[ -n "$ver" ] || { echo "stamp.sh: cannot read helperVersion from version.go" >&2; exit 1; }
printf '%s\n' "$ver" > dist/VERSION

echo "stamped dist/install.sh (${ver}) — its sums are the binaries beside it"
