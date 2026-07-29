#!/bin/sh
# codex-meter installer.
#
#   curl -fsSL https://tools.subnsub.com/meter/install.sh | sh -s -- <TOKEN>
#
# Source: https://github.com/subnsub-tools/codex-meter (Apache-2.0)
#
# …and if you would rather look first, which is the reasonable instinct for
# anything that installs a background process:
#
#   curl -fsSL https://tools.subnsub.com/meter/install.sh -o install.sh
#   less install.sh && sh install.sh <TOKEN>
#
# Installs a single static binary to ~/.local/bin and registers it to run at
# login — systemd user unit on Linux, LaunchAgent on macOS. No sudo, nothing
# written outside your home directory.
#
# Uninstall:  sh install.sh --uninstall
set -eu

RELAY="${CM_RELAY:-https://meter.subnsub.com}"
BASE="${CM_BASE:-https://tools.subnsub.com/meter}"   # where the binaries live
NAME=codex-meter
BINDIR="${CM_BINDIR:-$HOME/.local/bin}"
LABEL=com.subnsub.codex-meter   # shows up in `launchctl list`; brand domain there too

# Expected SHA-256 of each published binary, baked in.
#
# This is the whole point of the script being readable before you run it. A
# checksum fetched from the same host the binary came from proves nothing —
# whoever can swap one can swap both. These live here, in the file you were
# invited to read, and are regenerated alongside each upload by the release
# script that publishes the binaries. A binary that does not match is not installed, and a swapped binary
# would otherwise be free to read ~/.claude/.credentials.json and post it
# somewhere, which no amount of care in the Go source can prevent.
SUM_linux_amd64=df94dff81c2e9019c776c14c5aefdbf981e3a374cef5603e4b9d43f88966456c
SUM_linux_arm64=8634ff063a96a3d1870c0c8eb676fdd6dd221fa960970f1ad793475f8cf28a1d
SUM_darwin_amd64=6c7c93059c848c868e9ffbbe16bc67f4df86ff3f84787ab7a9f202294b676bb7
SUM_darwin_arm64=f247fbe147e33aaad92b7cc78cca76873b9dfc67f59ee7008dec7286df26e922

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- uninstall
uninstall() {
    # Where the install actually put things, not where a default would guess.
    # Without this, uninstalling after `CM_BINDIR=/opt/bin sh install.sh` would
    # delete an unrelated ~/.local/bin/codex-meter and leave the real one
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
    rm -f "${INSTALLED_BIN:-$BINDIR/$NAME}" "$HOME/.config/$NAME/token" "$manifest"
    rmdir "$HOME/.config/$NAME" 2>/dev/null || true
    say "removed $NAME"
    exit 0
}
[ "${1:-}" = "--uninstall" ] && uninstall

# ------------------------------------------------------------------- token
TOKEN="${1:-${CODEX_METER_TOKEN:-}}"
[ -n "$TOKEN" ] || die "no token. Usage: sh install.sh <TOKEN>"

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

if [ -n "${CM_LOCAL:-}" ]; then
    # Install from a binary you already have — used while the download host
    # is still being set up, and handy for testing a build before publishing.
    say "installing from $CM_LOCAL"
    cp "$CM_LOCAL" "$tmp"
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

# Verify before anything is made executable. Skipped only for CM_LOCAL, where
# you supplied the file yourself and there is nothing to authenticate.
if [ -z "${CM_LOCAL:-}" ]; then
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
conf="$HOME/.config/$NAME"
mkdir -p "$conf"
umask 077
# Write to a fresh name and rename over the target. A plain `>` redirect
# follows an existing symlink to wherever it points, blocks forever on a FIFO,
# and keeps whatever mode the old file had until the chmod lands — by which
# time the secret has already been written under it.
rm -f "$conf/token.new"
printf 'CODEX_METER_TOKEN=%s\n' "$TOKEN" > "$conf/token.new"
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

# ------------------------------------------------------------------ service
case "$goos" in
  linux)
    unitdir="$HOME/.config/systemd/user"
    mkdir -p "$unitdir"
    cat > "$unitdir/$NAME.service" <<EOF
[Unit]
Description=codex-meter — pushes AI coding quota to $RELAY
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
  <dict><key>CODEX_METER_TOKEN</key><string>$TOKEN</string></dict>
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
