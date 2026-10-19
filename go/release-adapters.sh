#!/bin/sh
# Publish a newer adapters.json to every helper in the field, without a build.
#
#   sh helper/go/release-adapters.sh            (dry run: test, sign, compare)
#   sh helper/go/release-adapters.sh --upload   (and publish)
#
# What a helper accepts is a table whose `version` is above the one it holds
# and whose signature checks against the key compiled into it (adapterfeed.go).
# So this refuses to publish a version that is not above the live one, and it
# refuses to sign with a key whose public half is not the one in the source —
# a table signed by the wrong key would be fetched, refused and logged on every
# machine, which is a worse outcome than an error here.
#
# The release key lives at $ADAPTERS_KEY (default below), 0600, on this
# machine only. `go run ./tools/signadapters keygen <path>` makes one; the
# public half it prints goes into adapterfeed.go, and THAT is a helper release.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
table="$here/adapters.json"
key="${ADAPTERS_KEY:-$HOME/.config/subnsub-release/adapters.key}"
base="https://tools.subnsub.com/monitor"
# Like build.sh: the developer's own Go before whatever is on PATH, because
# the system one here is 1.18 and cannot even read go.mod (2026-10-07: the
# first release with the table at go 1.27 failed on exactly that).
go="${GO:-}"
[ -n "$go" ] || { [ -x "$HOME/.local/go/bin/go" ] && go="$HOME/.local/go/bin/go"; } || go=go
export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
# `go run ./tools/…` needs the module; everything below runs from helper/go and
# names the repo root explicitly when it needs it.
cd "$here"

[ -f "$key" ] || { echo "no release key at $key" >&2; exit 1; }

ver=$(python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1]))["version"]))' "$table")
live=$(curl -fsS "$base/adapters.json" 2>/dev/null | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["version"]))' 2>/dev/null || echo 0)
echo "table version:   $ver (published: $live)"
[ "$ver" -gt "$live" ] || { echo "adapters.json version $ver is not above the published $live — bump it" >&2; exit 1; }

pub=$("$go" run ./tools/signadapters pubkey "$key")
grep -q "adapterFeedPubKey = \"$pub\"" "$here/adapterfeed.go" || {
    echo "the key at $key is not the one adapterfeed.go trusts" >&2; exit 1; }

# The table has to read every fixture the tests pin before it goes anywhere.
# RELEASE_PREBUILT=1 means the whole suite already ran on the build machine
# (release.sh takes the same word for the same reason: the host that holds
# the upload credentials is a loaded production box), so only the signing
# happens here.
if [ "${RELEASE_PREBUILT:-}" = 1 ]; then
    echo "prebuilt:        taking the tests as run elsewhere"
else
    "$go" test -run 'Adapter|Claude|Codex|Antigravity|Kimi|Xai|CPA' . >/dev/null || {
        echo "the adapter tests do not pass with this table" >&2; exit 1; }
fi

mkdir -p "$here/dist"
"$go" run ./tools/signadapters sign "$key" "$table" > "$here/dist/adapters.sig"
"$go" run ./tools/signadapters verify "$pub" "$table" "$here/dist/adapters.sig" >/dev/null
echo "signed:          dist/adapters.sig"

[ "${1:-}" = "--upload" ] || { echo "(dry run — pass --upload to publish)"; exit 0; }
cd "$repo"
set -a; . "$repo/.env.cloudflare"; set +a
unset CLOUDFLARE_API_TOKEN CF_API_TOKEN
npx --no-install wrangler r2 object put "link/monitor/adapters.sig" --file "$here/dist/adapters.sig" \
    --content-type text/plain --remote >/dev/null
npx --no-install wrangler r2 object put "link/monitor/adapters.json" --file "$table" \
    --content-type application/json --remote >/dev/null
echo "  uploaded adapters.json + adapters.sig"
sleep 3
now=$(curl -fsS "$base/adapters.json" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["version"]))')
[ "$now" = "$ver" ] && echo "live:            v$now" || { echo "live table reads v$now, expected v$ver" >&2; exit 1; }
