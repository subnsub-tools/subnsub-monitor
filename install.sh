#!/bin/sh
# subnsub-monitor installer.
#
#   curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN>
#
# Paste the same token on every machine you want to watch — each one gets its
# own dashboard, told apart by a random id it creates on first run. To name
# one, add it after the token (or set MON_NAME):
#
#   curl -fsSL .../install.sh | sh -s -- <TOKEN> --name "tokyo build box"
#
# Source: https://github.com/subnsub-tools/subnsub-monitor (Apache-2.0)
#
# …and if you would rather look first, which is the reasonable instinct for
# anything that installs a background process:
#
#   curl -fsSL https://tools.subnsub.com/monitor/install.sh -o install.sh
#   less install.sh && sh install.sh <TOKEN>
#
# Installs a single static binary to ~/.local/bin and registers it to run at
# login — systemd user unit on Linux, LaunchAgent on macOS. No sudo, nothing
# written outside your home directory.
#
# Uninstall:  sh install.sh --uninstall
set -eu

RELAY="${MON_RELAY:-https://monitor.subnsub.com}"
BASE="${MON_BASE:-https://tools.subnsub.com/monitor}"   # where the binaries live
NAME=subnsub-monitor
BINDIR="${MON_BINDIR:-$HOME/.local/bin}"
LABEL=com.subnsub.monitor   # shows up in `launchctl list`; brand domain there too

# Expected SHA-256 of each published binary, baked in.
#
# This is the whole point of the script being readable before you run it. A
# checksum fetched from the same host the binary came from proves nothing —
# whoever can swap one can swap both. These live here, in the file you were
# invited to read, and are regenerated alongside each upload by the release
# script that publishes the binaries. A binary that does not match is not installed, and a swapped binary
# would otherwise be free to read ~/.claude/.credentials.json and post it
# somewhere, which no amount of care in the Go source can prevent.
SUM_linux_amd64=7464ef08d9101355ef75e09607179f99e3084f6102c1da91bd7bcd8bf6d75262
SUM_linux_arm64=6bcc71fb468a3f3df0497749315d3eeac3a483ed8406ac3d87ef22d6aefbb0a8
SUM_darwin_amd64=32dd69869a74427604d7f4e3199d048bb5171bc9915bce1b74f6396f7c5f2af1
SUM_darwin_arm64=c1bf95406b4d93c223cf293172066d10b74598dd51a856d50af929993c715f89

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- uninstall
uninstall() {
    # Where the install actually put things, not where a default would guess.
    # Without this, uninstalling after `MON_BINDIR=/opt/bin sh install.sh` would
    # delete an unrelated ~/.local/bin/subnsub-monitor and leave the real one
    # running.
    manifest="$HOME/.config/$NAME/manifest"
    if [ -f "$manifest" ]; then
        # shellcheck disable=SC1090
        . "$manifest"
        [ -n "${INSTALLED_BIN:-}" ] && BINDIR=$(dirname "$INSTALLED_BIN")
    fi
    case "$(uname -s)" in
      Darwin)
        plist="$HOME/Library/LaunchAgents/${INSTALLED_LABEL:-$LABEL}.plist"
        [ -f "$plist" ] && launchctl unload "$plist" 2>/dev/null || true
        rm -f "$plist"
        ;;
      Linux)
        systemctl --user disable --now "$NAME.service" 2>/dev/null || true
        rm -f "$HOME/.config/systemd/user/$NAME.service"
        systemctl --user daemon-reload 2>/dev/null || true
        ;;
    esac
    # agent-id and name go too: leaving the id behind means a reinstall
    # silently reclaims the same dashboard card, which is surprising when the
    # point of uninstalling was to stop watching this machine.
    # token.current is the renewed token the helper writes for itself; it is a
    # bearer secret like the installed one and has to go with it. Leaving it
    # behind would also mean a later reinstall silently resurrecting the old
    # credential instead of using the token just pasted.
    # token.current.new too: it is the write-then-rename staging name, and a
    # crash between the two steps leaves a readable bearer secret under it that
    # nothing else would ever clean up.
    rm -f "${INSTALLED_BIN:-$BINDIR/$NAME}" "$HOME/.config/$NAME/token" \
          "$HOME/.config/$NAME/token.current" "$HOME/.config/$NAME/token.current.new" \
          "$HOME/.config/$NAME/agent-id" "$HOME/.config/$NAME/name" "$manifest"
    rmdir "$HOME/.config/$NAME" 2>/dev/null || true
    say "removed $NAME"
    exit 0
}
[ "${1:-}" = "--uninstall" ] && uninstall

# -------------------------------------------------------------- token / name
# The token is the first bare argument, or the environment. Checked for a
# leading dash first: with SUBNSUB_MONITOR_TOKEN already set,
# `sh install.sh --name box` would otherwise take "--name" as the token and
# fail with a bewildering complaint about the characters in it.
TOKEN=""
case "${1:-}" in
  ''|-*) TOKEN="${SUBNSUB_MONITOR_TOKEN:-}" ;;
  *)     TOKEN=$1; shift ;;
esac
[ -n "$TOKEN" ] || die "no token. Usage: sh install.sh <TOKEN> [--name LABEL]"

# What this machine is called on the dashboard. Optional, and deliberately not
# guessed: the helper never reads the hostname, so an unnamed machine shows up
# under its own random id rather than under whatever the box calls itself.
NAME_ARG="${MON_NAME:-}"
while [ $# -gt 0 ]; do
    case "$1" in
      --name) [ $# -ge 2 ] || die "--name needs a value"; NAME_ARG=$2; shift 2 ;;
      --name=*) NAME_ARG=${1#--name=}; shift ;;
      *) die "unexpected argument: $1" ;;
    esac
done

# Same shape the relay accepts. Beyond catching typos this is what keeps a
# token with a newline in it from adding arbitrary lines to the systemd
# EnvironmentFile, or one with a '<' from breaking out of a plist string.
# Everything downstream interpolates this value into a config file.
case "$TOKEN" in
  *[!A-Za-z0-9_-]*) die "token has characters outside [A-Za-z0-9_-]" ;;
esac
len=${#TOKEN}
[ "$len" -ge 24 ] && [ "$len" -le 128 ] || die "token must be 24-128 characters"

# The relay URL is interpolated into the same files. Keep it to an https URL
# made of characters that cannot terminate an XML string or a unit directive.
case "$RELAY" in
  https://*) ;;
  *) die "relay must be an https:// URL" ;;
esac
case "$RELAY" in
  *[!A-Za-z0-9:/._-]*) die "relay URL has unexpected characters" ;;
esac

# ------------------------------------------------------------ os / arch
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  goos=linux  ;;
  Darwin) goos=darwin ;;
  *) die "unsupported OS: $os (Linux and macOS only)" ;;
esac
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
asset="$NAME-$goos-$goarch"

# ----------------------------------------------------------------- fetch
mkdir -p "$BINDIR"
tmp=$(mktemp "${TMPDIR:-/tmp}/$NAME.XXXXXX")
# Clean up on any exit path, including the failure ones below.
trap 'rm -f "$tmp"' EXIT INT TERM

if [ -n "${MON_LOCAL:-}" ]; then
    # Install from a binary you already have — used while the download host
    # is still being set up, and handy for testing a build before publishing.
    say "installing from $MON_LOCAL"
    cp "$MON_LOCAL" "$tmp"
else
    say "downloading $asset"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$BASE/$asset" -o "$tmp" || die "download failed: $BASE/$asset"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$tmp" "$BASE/$asset" || die "download failed: $BASE/$asset"
    else
        die "need curl or wget"
    fi
fi

[ -s "$tmp" ] || die "downloaded file is empty"

# Verify before anything is made executable. Skipped only for MON_LOCAL, where
# you supplied the file yourself and there is nothing to authenticate.
if [ -z "${MON_LOCAL:-}" ]; then
    eval "want=\${SUM_${goos}_${goarch}}"
    case "$want" in
      PLACEHOLDER_*) die "this installer has no checksum for $goos/$goarch — refusing to run an unverified binary" ;;
    esac
    if command -v sha256sum >/dev/null 2>&1; then
        got=$(sha256sum < "$tmp" | cut -d' ' -f1)
    elif command -v shasum >/dev/null 2>&1; then
        got=$(shasum -a 256 < "$tmp" | cut -d' ' -f1)
    else
        die "need sha256sum or shasum to verify the download"
    fi
    [ "$got" = "$want" ] || die "checksum mismatch — expected $want, got $got. NOT installing."
    say "checksum ok"
fi

# Run it BEFORE it replaces anything. A binary that downloads and verifies but
# cannot execute here (wrong libc, wrong arch reported by uname) must not take
# out a working install on its way past.
chmod 0755 "$tmp"
"$tmp" token >/dev/null 2>&1 || die "the downloaded binary does not run on this machine"

# Land it in the target directory first so the final step is a rename within
# one filesystem — atomic, and it cannot leave a half-written binary behind.
staged="$BINDIR/.$NAME.new.$$"
mkdir -p "$BINDIR"
cp "$tmp" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$BINDIR/$NAME"
rm -f "$tmp"
trap - EXIT INT TERM
say "installed $BINDIR/$NAME"

# ----------------------------------------------------------------- token file
# Kept out of the service definition and out of the process arguments, so it
# does not show up in `ps` or in a world-readable unit file.
#
# This is the BOOTSTRAP token and it is not the last word. Tokens expire after
# 30 days, and the helper trades its own in for a fresh one before that, writing
# the replacement to `token.current` beside this file — a place it reads itself,
# because this file is a systemd EnvironmentFile that only a restart re-reads
# and on macOS is not consulted at all (the token is inlined in the plist).
# Re-running this installer with a newly pasted token still wins: the helper
# picks whichever of the two lasts longer, and a just-issued one always does.
conf="$HOME/.config/$NAME"
mkdir -p "$conf"
umask 077
# Write to a fresh name and rename over the target. A plain `>` redirect
# follows an existing symlink to wherever it points, blocks forever on a FIFO,
# and keeps whatever mode the old file had until the chmod lands — by which
# time the secret has already been written under it.
rm -f "$conf/token.new"
printf 'SUBNSUB_MONITOR_TOKEN=%s\n' "$TOKEN" > "$conf/token.new"
chmod 0600 "$conf/token.new"
mv -f "$conf/token.new" "$conf/token"

# Record what this run actually installed, so --uninstall removes THIS
# install rather than whatever the defaults would point at today.
rm -f "$conf/manifest.new"
{
    printf 'INSTALLED_BIN=%s\n' "$BINDIR/$NAME"
    printf 'INSTALLED_LABEL=%s\n' "$LABEL"
    printf 'INSTALLED_RELAY=%s\n' "$RELAY"
} > "$conf/manifest.new"
chmod 0644 "$conf/manifest.new"
mv -f "$conf/manifest.new" "$conf/manifest"

# ------------------------------------------------------------------ name file
# Written as a file rather than passed through the service definition on
# purpose: a name is free text — spaces, quotes, CJK — and interpolating that
# into a systemd unit or a plist is how a label becomes a parse error, or
# worse. The helper reads it, trims it and drops control characters; nothing
# here has to be escaped because nothing here is a template.
if [ -n "$NAME_ARG" ]; then
    rm -f "$conf/name.new"
    printf '%s\n' "$NAME_ARG" > "$conf/name.new"
    chmod 0600 "$conf/name.new"
    mv -f "$conf/name.new" "$conf/name"
fi

# Materialise this machine's dashboard id now, out here, rather than leaving it
# to the first run of the service. The service is sandboxed (see the unit
# below) and on a host where that sandbox is actually enforced it cannot write
# to $HOME — so the id would be regenerated on every start and the machine
# would arrive at the relay as a brand new one after each restart. Creating it
# here means the ordinary path never needs a runtime write at all.
"$BINDIR/$NAME" name >/dev/null 2>&1 || true

# ------------------------------------------------------------------ service
case "$goos" in
  linux)
    unitdir="$HOME/.config/systemd/user"
    mkdir -p "$unitdir"
    cat > "$unitdir/$NAME.service" <<EOF
[Unit]
Description=subnsub-monitor — pushes AI coding quota to $RELAY
After=network-online.target

[Service]
Type=simple
EnvironmentFile=$conf/token
ExecStart=$BINDIR/$NAME connect $RELAY
Restart=always
RestartSec=30
# It reads two files and makes one outbound request; it needs nothing else.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
# …with one hole, and only this one: the directory holding this machine's
# dashboard id. Without it, a host that actually enforces the sandbox above
# gives the helper a read-only \$HOME, the id cannot be written, and every
# restart shows up at the relay as a different machine. The leading '-' makes
# it optional, so deleting the directory downgrades the helper rather than
# leaving a unit that refuses to start.
ReadWritePaths=-$conf
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=default.target
EOF
    if command -v systemctl >/dev/null 2>&1; then
        systemctl --user daemon-reload
        systemctl --user enable --now "$NAME.service"
        # `enable --now` starts a stopped unit but leaves a running one alone,
        # so reinstalling over an existing install would keep the OLD binary,
        # token and relay URL running while reporting success.
        systemctl --user restart "$NAME.service"
        say "started (systemctl --user status $NAME)"
        # Without lingering, the user manager stops at logout and takes the
        # helper with it — exactly wrong on a VPS you SSH into occasionally.
        if command -v loginctl >/dev/null 2>&1; then
            loginctl enable-linger "$(id -un)" 2>/dev/null \
              || say "note: could not enable lingering; it will stop when you log out."
        fi
    else
        say "no systemctl here — run manually:  $BINDIR/$NAME connect $RELAY"
    fi
    ;;

  darwin)
    plistdir="$HOME/Library/LaunchAgents"
    mkdir -p "$plistdir"
    cat > "$plistdir/$LABEL.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BINDIR/$NAME</string>
    <string>connect</string>
    <string>$RELAY</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>SUBNSUB_MONITOR_TOKEN</key><string>$TOKEN</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
</dict>
</plist>
EOF
    chmod 0600 "$plistdir/$LABEL.plist"
    launchctl unload "$plistdir/$LABEL.plist" 2>/dev/null || true
    launchctl load "$plistdir/$LABEL.plist"
    say "started (launchctl list | grep $LABEL)"
    ;;
esac

say ""
say "pushing to $RELAY every 30s."
say "check locally:  $BINDIR/$NAME"
say "uninstall:      sh install.sh --uninstall"
