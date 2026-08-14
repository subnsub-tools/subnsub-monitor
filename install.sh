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
# The dashboard can also run commands on a machine, and that is OFF unless you
# ask for it right here — add --console, or turn it on later with
# `subnsub-monitor console on`. With it on, this machine runs what you type on
# the dashboard, as this user, and writes every command to its own log first.
#
# And it can update the helper for you, so a new release does not mean logging
# into every box. Also off by default: add --remote-update, or turn it on later
# with `subnsub-monitor update on`. --console implies it — a console can
# already run this installer. Nothing is downloaded until you press the button:
# there is no timer, and what gets installed comes from the release bucket and
# is checked against its published checksum, so the dashboard chooses only WHEN.
#
# The dashboard can also SEARCH this machine's coding-agent sessions, if you add
# --sessions (which needs --console — the search runs over the console channel).
# It installs AgentsView, a session index pinned to a reviewed version and
# checksummed the same way; the search runs on this machine and only matching
# snippets ever leave it. Turn it on later instead with
# `subnsub-monitor sessions install`.
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
# The dashboard key allowed to command this machine, if the line you copied
# carries one. It is a PUBLIC key: the private half never leaves the browser
# that made it, and the relay never sees either. Pinning it here is what
# turns "the relay could tell this machine to run something" into "only a
# browser holding that key can" — see helper/go/signing.go.
PUBKEY="${MON_PUBKEY:-}"
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
SUM_linux_amd64=a16c724c3655923fec78743b8dd3fa34da2afa42872d1a473d2b37e3d25487f9
SUM_linux_arm64=c92508a441bd032aae5d9fc416fba17d61d56582053cdb783709df5a6ce132e6
SUM_linux_arm=da638aa698b79a6d4fe87ee736e65ce9c909a26b8f6e502c9577627d45f68041
SUM_darwin_amd64=e94e6d9ecc67883ecebd16616cd910ec4aa9942aae55f7d38133ebfa7075da40
SUM_darwin_arm64=8d5c12f1d5f15a26469f36937ede2b6e9aa9ddf58915f8a3b4401b6be4f08833
SUM_freebsd_amd64=6684f7e705ad57f87ac8786bc16c987fdb0d11d8773f11515a8e5eadabc0ea4b
SUM_freebsd_arm64=a23426ffdcc415ddb9e04d3becc358a5afa9d6b1c8e724ef29e9540c460bdb96

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
          "$HOME/.config/$NAME/agent-id" "$HOME/.config/$NAME/name" \
          "$HOME/.config/$NAME/console" "$HOME/.config/$NAME/update" \
          "$HOME/.config/$NAME/diagnostics" "$manifest"
    # The binary the last self-update moved aside. Left behind it would be an
    # unreferenced few megabytes under a name nothing else will ever clean up.
    rm -f "${INSTALLED_BIN:-$BINDIR/$NAME}.prev"
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
[ -n "$TOKEN" ] || die "no token. Usage: sh install.sh <TOKEN> [--name LABEL] [--console] [--remote-update] [--sessions]"

# What this machine is called on the dashboard. Optional, and deliberately not
# guessed: the helper never reads the hostname, so an unnamed machine shows up
# under its own random id rather than under whatever the box calls itself.
NAME_ARG="${MON_NAME:-}"
# Whether the dashboard may run commands on this machine. OFF unless asked for
# here, on the machine, by whoever is running this script — see console.go for
# why that is the only place the decision can honestly be made.
CONSOLE_ARG=""
# Whether the dashboard may replace this helper's binary. Same rule, same
# place, and a separate switch from the console even though the console implies
# it — see helper/go/update.go. A machine that wants the button and no shell is
# the case this exists for.
UPDATE_ARG=""
# Whether to also install AgentsView, the session index the dashboard's fleet
# search reads. OFF unless asked for, and gated on --console below: the search
# runs over the console channel, so the index is only useful where that is on.
SESSIONS_ARG=""
while [ $# -gt 0 ]; do
    case "$1" in
      --name) [ $# -ge 2 ] || die "--name needs a value"; NAME_ARG=$2; shift 2 ;;
      --name=*) NAME_ARG=${1#--name=}; shift ;;
      --console) CONSOLE_ARG=on; shift ;;
      --console=on|--console=yes|--console=1) CONSOLE_ARG=on; shift ;;
      --console=off|--console=no|--console=0) CONSOLE_ARG=off; shift ;;
      --remote-update) UPDATE_ARG=on; shift ;;
      --remote-update=on|--remote-update=yes|--remote-update=1) UPDATE_ARG=on; shift ;;
      --remote-update=off|--remote-update=no|--remote-update=0) UPDATE_ARG=off; shift ;;
      --sessions) SESSIONS_ARG=on; shift ;;
      --sessions=on|--sessions=yes|--sessions=1) SESSIONS_ARG=on; shift ;;
      --sessions=off|--sessions=no|--sessions=0) SESSIONS_ARG=off; shift ;;
      *) die "unexpected argument: $1" ;;
    esac
done

# --sessions installs AgentsView, the per-machine session index the dashboard's
# fleet search runs. The search only ever reaches a machine whose console is on
# (it IS a console command), so asking for the index without the channel that
# uses it would install 100 MB that nothing here can call. Required rather than
# implied: turning the console on is a real grant and should be typed, not
# arrive as a side effect of wanting search.
if [ "$SESSIONS_ARG" = on ] && [ "$CONSOLE_ARG" != on ]; then
    die "--sessions needs --console: the fleet search reaches a machine only through its console"
fi

# Same shape the relay accepts. Beyond catching typos this is what keeps a
# token with a newline in it from adding arbitrary lines to the systemd
# EnvironmentFile, or one with a '<' from breaking out of a plist string.
# Everything downstream interpolates this value into a config file.
case "$TOKEN" in
  *[!A-Za-z0-9_-]*) die "token has characters outside [A-Za-z0-9_-]" ;;
esac
len=${#TOKEN}
[ "$len" -ge 24 ] && [ "$len" -le 128 ] || die "token must be 24-128 characters"

# Base64, and the length Ed25519 public keys have. Checked here rather than
# only in the helper because this value is written into a config file, and
# "whatever was in the environment" is not a thing to write into one.
# A 32-byte key in standard base64 is exactly 43 characters and one trailing
# '=' — nothing else is that shape. Checking the shape rather than "44 of the
# right characters" matters because everything that gets past here is written
# into the trusted-keys file, and a value the HELPER cannot decode leaves that
# file existing with no usable key in it: the machine then refuses every
# command until somebody fixes it by hand. Better to refuse the install line
# that carried it, here, while there is a person watching.
if [ -n "$PUBKEY" ]; then
  case "$PUBKEY" in
    *[!A-Za-z0-9+/=]*) die "MON_PUBKEY has characters outside base64" ;;
  esac
  case "$PUBKEY" in
    *=) ;;
    *) die "MON_PUBKEY is not a 32-byte Ed25519 key in base64" ;;
  esac
  case "${PUBKEY%=}" in
    *[!A-Za-z0-9+/]*) die "MON_PUBKEY is not a 32-byte Ed25519 key in base64" ;;
  esac
  [ "${#PUBKEY}" -eq 44 ] || die "MON_PUBKEY is not a 32-byte Ed25519 key in base64"
fi

# The relay URL is interpolated into the same files. Keep it to an https URL
# made of characters that cannot terminate an XML string or a unit directive.
case "$RELAY" in
  https://*) ;;
  *) die "relay must be an https:// URL" ;;
esac
case "$RELAY" in
  *[!A-Za-z0-9:/._-]*) die "relay URL has unexpected characters" ;;
esac

# Where tokens get renewed, for people running their own relay.
#
# The helper renews only against the site this build ships with, or one named
# here — it will not offer a token minted by somebody else's issuer to ours. So
# a custom relay needs this, and it has to survive into the SERVICE: the value
# was only ever in the installing shell's environment, and the unit and the
# plist below carry nothing but the token, so without persisting it the
# background process would come up with renewal quietly switched off. An
# install that looked like it worked and stopped reporting a month later.
SITE="${SUBNSUB_MONITOR_SITE:-}"
if [ -n "$SITE" ]; then
    case "$SITE" in
      https://*) ;;
      *) die "SUBNSUB_MONITOR_SITE must be an https:// URL" ;;
    esac
    # Same interpolation-safety rule as the relay: this value is written into a
    # systemd EnvironmentFile and an XML string.
    case "$SITE" in
      *[!A-Za-z0-9:/._-]*) die "SUBNSUB_MONITOR_SITE has unexpected characters" ;;
    esac
fi

# ------------------------------------------------------------ os / arch
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)   goos=linux   ;;
  Darwin)  goos=darwin  ;;
  FreeBSD) goos=freebsd ;;
  # Somebody running this inside Git Bash, MSYS2, Cygwin or the like. Named
  # separately from the catch-all below so the answer can be the true one:
  # Windows is not a platform this is published for, rather than one nobody
  # thought about. The port exists and is not being offered.
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    die "Windows is not supported. No binary is published for it, and this
installer would not register anything that survives a reboot there." ;;
  *) die "unsupported OS: $os (Linux, macOS and FreeBSD)" ;;
esac
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  # 32-bit ARM: a Raspberry Pi running the 32-bit OS, and a fair number of
  # cheap boxes. armv6 is NOT accepted — the published build is ARMv7 and would
  # fault on a Pi 1 or a Zero W, which is a worse outcome than being told so.
  armv7l|armv7|armhf) goarch=arm ;;
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
{
    printf 'SUBNSUB_MONITOR_TOKEN=%s\n' "$TOKEN"
    # Renewal site, when one was named. Rides in the same file because it has
    # to reach the service, and systemd reads only this one.
    [ -n "$SITE" ] && printf 'SUBNSUB_MONITOR_SITE=%s\n' "$SITE"
    true
} > "$conf/token.new"
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

# --------------------------------------------------------------- console file
# The switch that lets the dashboard run commands here. Its EXISTENCE is the
# setting; nothing reads what is in it. Written only when asked for, and
# removed only when explicitly turned off — a reinstall without --console
# leaves an operator's earlier decision alone rather than silently reversing it
# in either direction.
case "$CONSOLE_ARG" in
  on)  : > "$conf/console"; chmod 0600 "$conf/console" ;;
  off) rm -f "$conf/console" ;;
esac

# ---------------------------------------------------------------- update file
# The same shape for the same reasons. Note it is NOT written when --console is
# given: the console allows this on its own, and writing a second file would
# mean `console off` later left behind a switch the operator never set.
case "$UPDATE_ARG" in
  on)  : > "$conf/update"; chmod 0600 "$conf/update" ;;
  off) rm -f "$conf/update" ;;
esac

# --------------------------------------------------------- trusted key file
# APPENDED, never overwritten: re-running the installer from a second browser
# is how a second key gets added, and truncating here would silently revoke the
# first one — locking whoever is not at this keyboard out of their own machine.
# Duplicates are harmless; the helper matches any pinned key.
if [ -n "$PUBKEY" ]; then
  if ! grep -qxF "$PUBKEY" "$conf/trusted-keys" 2>/dev/null; then
    printf '%s\n' "$PUBKEY" >> "$conf/trusted-keys"
  fi
  chmod 0600 "$conf/trusted-keys"
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
    # How much of the machine this service can touch, and it depends on whether
    # the console is on — because those are two different programs in every way
    # that matters to a sandbox.
    #
    # Without the console it reads two files and makes one outbound request, so
    # it is confined to almost nothing. With the console it runs commands the
    # operator types, and a confinement that made `mkdir` fail would not be
    # security, it would be a console that does not work. The operator asked
    # for a shell on this box; giving them one that cannot write anything and
    # not saying so is the dishonest option.
    #
    # ProtectHome/ProtectSystem apply to the WHOLE cgroup, children included,
    # so there is no version of this where the helper stays confined and the
    # commands it spawns do not. Hence the either/or.
    if [ -f "$conf/console" ]; then
        SANDBOX="# The console is on: commands you type on the dashboard run in this
# service's cgroup, so the filesystem confinement below is deliberately the
# ordinary one for a user service. NoNewPrivileges still holds — nothing here
# can gain privileges this user does not already have — and turning the console
# off (subnsub-monitor console off, then reinstall) puts the strict sandbox back.
NoNewPrivileges=true
ProtectKernelTunables=true
RestrictSUIDSGID=true"
    else
        # The strict sandbox, with one hole — or two, when this machine is
        # allowed to update itself.
        #
        # ProtectHome=read-only means the helper cannot write its own binary,
        # which is the correct default and is also exactly what a self-update
        # has to do. So the second hole is opened only for a machine whose
        # operator asked for that, and it is opened as narrowly as the
        # directory allows: $BINDIR, not \$HOME.
        #
        # This is a real widening and worth being clear-eyed about — it makes
        # the one file the service executes writable by the service. What keeps
        # that honest is on the other side: the payload comes from the release
        # bucket over TLS and must match the published checksum, and nothing
        # downloads until somebody presses a button. See helper/go/update.go.
        UPD_HOLE=""
        [ -f "$conf/update" ] && UPD_HOLE="
# This machine may update itself, so the directory holding the binary it
# replaces has to be writable by it. Nothing else in \$HOME is.
ReadWritePaths=-$BINDIR"
        SANDBOX="# It reads two files and makes one outbound request; it needs nothing else.
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
ReadWritePaths=-$conf$UPD_HOLE
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true"
    fi
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
$SANDBOX
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
  <dict><key>SUBNSUB_MONITOR_TOKEN</key><string>$TOKEN</string>$([ -n "$SITE" ] && printf '<key>SUBNSUB_MONITOR_SITE</key><string>%s</string>' "$SITE")</dict>
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

  freebsd)
    # NOTHING IS REGISTERED HERE, and that is a decision rather than an
    # omission worth being explicit about.
    #
    # FreeBSD has no per-user service manager. The two ways to start something
    # at boot are an rc.d script, which lives in /usr/local/etc/rc.d and needs
    # root, and the user's crontab, which is not inside this script's promise
    # that nothing is written outside your home directory. Quietly doing
    # neither would be the worst of the three: an install that reports success
    # and stops the next time the machine reboots.
    #
    # So the binary, the token and the machine id are all in place, and the two
    # commands that finish the job are printed.
    say ""
    say "FreeBSD: no per-user service manager, so nothing was registered."
    say "start it now:     daemon -f $BINDIR/$NAME connect $RELAY"
    say "keep it at boot:  as root, drop this in /usr/local/etc/rc.d/subnsubmonitor"
    say "                  and add subnsubmonitor_enable=\"YES\" to /etc/rc.conf:"
    say ""
    say "                    #!/bin/sh"
    say "                    # PROVIDE: subnsubmonitor"
    say "                    # REQUIRE: NETWORKING"
    say "                    # KEYWORD: shutdown"
    say "                    . /etc/rc.subr"
    say "                    name=subnsubmonitor"
    say "                    rcvar=subnsubmonitor_enable"
    say "                    command=$BINDIR/$NAME"
    say "                    command_args=\"connect $RELAY\""
    say "                    subnsubmonitor_user=$(id -un)"
    # HOME has to be set explicitly: rc.subr keeps root's environment when it
    # drops privileges, and everything this helper owns — the token, the machine
    # id, the name — is under ~/.config. Without it the service comes up as a
    # brand new unnamed machine on every boot, or does not authenticate at all.
    say "                    subnsubmonitor_env=\"HOME=$HOME\""
    say "                    load_rc_config \$name"
    say "                    run_rc_command \"\$1\""
    ;;
esac

# ------------------------------------------------------------ session index
# Done here, after the binary is in place and the service is up, because it
# runs THAT binary (`sessions install`) to do the download — the same pinned,
# checksummed fetch the dashboard would trigger, so the installer and the
# dashboard share one code path rather than two that can drift. A failure here
# does not fail the install: the helper is running and pushing quota; the
# index is an add-on, and the message says how to retry.
SESSIONS_FAILED=0
if [ "$SESSIONS_ARG" = on ]; then
    say ""
    say "installing the AgentsView session index (for the dashboard's fleet search)…"
    if "$BINDIR/$NAME" sessions install; then
        :
    else
        # An EXPLICIT --sessions that failed is a failed request, and an
        # automated deploy has to be able to see that. The helper itself is up
        # and pushing, so this does not undo the install — but the script exits
        # non-zero at the end so the failure is not reported as success.
        SESSIONS_FAILED=1
        say "note: the session index did not install. The helper is running regardless;"
        say "      retry later with:  $BINDIR/$NAME sessions install"
    fi
fi

say ""
say "pushing to $RELAY every 30s."
say "check locally:  $BINDIR/$NAME"
# Stated on every install, not only when it was just switched on: a machine
# that can be typed at from a web page is something the person standing here
# should be told about, including when they inherited the setting from whoever
# installed it last.
if [ -f "$conf/console" ]; then
    say "console:        ON — the dashboard can run commands here as $(id -un)."
    say "                off with:  $BINDIR/$NAME console off"
else
    # Deliberately does not say "turn it on and you are done". On Linux the
    # unit written above is sandboxed read-only, and this switch cannot change
    # that — a console turned on afterwards can look but not write until the
    # installer runs again. `console on` says the same thing at the machine.
    say "console:        off. Turn it on with:  $BINDIR/$NAME console on"
    case "$goos" in
      linux) say "                (on Linux, re-run this installer with --console for a console that can write)" ;;
    esac
fi
# Stated on every install too, and for the same reason: "the dashboard can
# replace the program running here" is something the person standing at the
# machine should be told, not only the person who asked for it.
if [ -f "$conf/console" ]; then
    say "update:         ON — implied by the console, which can already run this installer."
elif [ -f "$conf/update" ]; then
    say "update:         ON — the dashboard can replace this helper. Nothing downloads"
    say "                until you press it, and only a checksummed release will install."
    say "                off with:  $BINDIR/$NAME update off"
else
    say "update:         off. Turn it on with:  $BINDIR/$NAME update on"
    case "$goos" in
      linux) say "                (on Linux, re-run this installer with --remote-update — the strict" ;;
    esac
    case "$goos" in
      linux) say "                 sandbox will not let the helper write its own binary otherwise)" ;;
    esac
fi
# Stated when the index is present, so the person at the machine knows the
# dashboard's fleet search can reach it — and, when it is not, how to add it.
# Reports the tool's own version rather than trusting that any executable at
# the path is the right one.
agentsview_ver=""
if [ -x "$HOME/.local/bin/agentsview" ]; then
    agentsview_ver=$("$HOME/.local/bin/agentsview" --version 2>/dev/null | head -n1 || true)
fi
if [ -n "$agentsview_ver" ]; then
    say "sessions:       index installed ($agentsview_ver) — the fleet search can reach this machine."
elif [ -f "$conf/console" ]; then
    say "sessions:       no session index. Add one with:  $BINDIR/$NAME sessions install"
fi
say "uninstall:      sh install.sh --uninstall"

# A machine explicitly asked to have the session index, but the install did
# not take: exit non-zero so an automated run does not read as fully done. Last
# thing in the script, after every message above has printed.
if [ "$SESSIONS_FAILED" = 1 ]; then
    exit 1
fi
