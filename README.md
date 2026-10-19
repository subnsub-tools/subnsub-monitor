# subnsub-monitor

The helper behind [SUB&SUB Monitor](https://tools.subnsub.com/monitor): one
static binary that runs on a machine you cannot reach, reads how much AI
coding quota is left there and whether the box itself is healthy, and gets
that in front of you without opening a port.

Two roads, and you choose:

```
  machine you can't reach                              you
  ┌──────────────────┐                           ┌───────────┐
  │  subnsub-monitor │──HTTPS push──▶ relay ◀──WS──│  browser  │   with an account
  │                  │                           │           │
  │                  │◀═══ WireGuard (tailcat) ══▶│           │   direct mode: no account,
  └──────────────────┘   meets at a DERP server  └───────────┘   no relay, no token
```

Nothing listens on the machine either way. The helper dials out: to our relay
over ordinary HTTPS, or to a DERP meeting point for an end-to-end encrypted
tunnel that only your browser can open.

This repository is the helper — its Go source, the installers, and ready-made
files for the tiles of your own it can carry. The dashboard is the hosted
page, and what the helper sends, what the relay holds and what nobody but you
can do is written up, claim by claim, on the
[trust & threat-model page](https://tools.subnsub.com/monitor-security).

## Credit where it is due

The idea — read the quota your AI coding tool has left on this machine, and
put it in front of you — comes from **[CodexBar](https://github.com/steipete/CodexBar)**
by [Peter Steinberger](https://github.com/steipete) (MIT).

CodexBar is a macOS menu-bar app. This is an independent implementation in a
different shape (a headless outbound-push agent), and contains no CodexBar
code — but the idea got here from there, and that is the part worth crediting.

## Install

**Linux, macOS, FreeBSD** — the published platforms (`linux/amd64`,
`linux/arm64`, `linux/arm`, `darwin/amd64`, `darwin/arm64`, `freebsd/amd64`,
`freebsd/arm64`, and `android/arm64` installed over USB, below).

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN>
```

…and if you would rather look first, which is the reasonable instinct for
anything that installs a background process:

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh -o install.sh
less install.sh && sh install.sh <TOKEN>
```

It installs one binary to `~/.local/bin` and registers it to run at login — a
systemd **user** unit on Linux, a LaunchAgent on macOS. No sudo, nothing
written outside your home directory. `sh install.sh --uninstall` removes it,
tells the relay the machine is gone, and leaves only what you may want to
keep (`trusted-keys`, the machine's own sealing key, its local history and
your `metrics.d/`); delete `~/.config/subnsub-monitor` yourself for a clean
slate.

The token comes from the Monitor page of a signed-in account: paste the same
one on every machine you want to watch, and each gets a card of its own.
Everything below the first line is optional:

| flag | what it turns on |
|---|---|
| `--name "tokyo build box"` | the name the card shows (never taken from the hostname) |
| `--console` | the dashboard may run commands here — off until you say so, see [The console](#the-console-and-why-it-is-off) |
| `--remote-update` | the dashboard may replace this helper with a newer release — implied by `--console` |
| `--auto-update` | the helper looks for a newer release hourly and installs it itself; counts only with remote updates on |
| `--sessions-sync` | this machine's coding-agent transcripts go to your account's archive — the one switch that sends something out rather than answering a question |
| `--direct --allow nodekey:…` | direct mode: an end-to-end encrypted tunnel for the browser whose key you name, no account needed — see [Direct mode](#direct-mode-no-account-no-relay-no-token) |
| `--derpmap https://…` | meet at a DERP server you run instead of the public tailcat map |

The install line the dashboard shows carries one more thing when you are
signed in: `MON_PUBKEY=…`, the public half of your account's command-signing
key. Pinning it on the machine is what lets your browser command it at all;
see [Who may command this machine](#who-may-command-this-machine).

The installer carries the expected SHA-256 of every published binary
**inline**. That is the whole reason it is worth reading before piping into a
shell: a checksum fetched at install time from the same host as the binary
authenticates nothing — whoever can swap one can swap the other. These live in
the file you were invited to read, and `install.sh` here is the same file the
site serves, re-stamped on every release.

**FreeBSD registers nothing**, and says so. There is no per-user service
manager: the two ways to start something at boot are an `rc.d` script, which
needs root, and your crontab, which is outside this installer's promise to
write nothing outside your home directory. The binary and the token are
installed and the installer prints the `daemon(8)` line and a ready-to-paste
`rc.d` script.

**OpenWrt** gets a boot entry written by the helper itself
(`subnsub-monitor autostart`) when the installer runs as root — the one write
outside a home directory, and only there.

> **Windows is not published.** The port is complete and the source is here
> (everything behind `//go:build windows`); `install.ps1` is here with its
> checksums set to placeholders so it declines rather than installs. The
> Windows notes below describe how it behaves when switched on, not something
> you can install today.

### Android, over USB

A phone is installed from the Monitor page, over a cable, and no other way.
The install line above is not for Android: run in a Termux session the helper
is an app, and Android lets an app see much less of the machine than Linux
does — no CPU percentage, load, disk throughput, network counters, process
count or temperature, by the platform's own policy. `install.sh` stops on
Android and says where to go.

With USB debugging on and the phone plugged into a desktop running Chrome or
Edge, the install card's "Android phone over USB" dialog does the install
itself: the page speaks ADB over WebUSB, fetches `subnsub-monitor-android-arm64`,
checks it against `SHA256SUMS`, copies it to the phone and starts it as the
shell user — which reads every metric Linux does, no root needed. It cannot
come back after a reboot; on a phone with Magisk, tick root and the same flow
installs under `/data/adb` with a `service.d` boot entry. The same dialog
takes it off again. Supported from Android 11; the helper decides at run
time, from what `/proc` shows, how much it may read. `MON_DNS=<ip>[,<ip>]`
overrides the resolver where Android's own daemon is refused;
`subnsub-monitor selftest` prints what the device lets the helper read.

## Direct mode: no account, no relay, no token

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- --direct --allow nodekey:<your browser's key>
```

The Monitor page's direct card generates a key for your browser and prints
that line. The helper opens an end-to-end encrypted WireGuard tunnel (the
tailcat data plane) that meets your browser at a DERP server; when the
install finishes the machine prints its own address, and you paste that into
the page. From then on the page dials the machine itself and the machine
answers it directly: the reading, a one-second health stream, its own
30-day history, and — once a key is pinned — the console and a terminal.

- **What touches our servers: nothing about the machine.** The DERP server
  carries ciphertext only. By default it is the public tailcat map; point
  `--derpmap` / `MON_DERPMAP` at one you run and the helper talks to nothing
  of ours. The page is a static viewer: what it shows lives on the machine
  and in that browser.
- **What you give up** is exactly the relay's half of the product: presence
  history, the relay-side trend, the session archive, web push, folding one
  account across machines. A machine the tunnel cannot reach shows nothing
  until it can.
- **Being allowed is a read-only view.** The tunnel does not start while no
  key is allowed (`allowed-clients`, one `nodekey:…` per line;
  `subnsub-monitor allow <key>` / `disallow <key>`), and a key struck from
  the list is refused from its next request. A command travels this road only
  from a browser whose signing key the machine has pinned (`MON_PUBKEY`,
  `trusted-keys`), which a `--direct` line alone does not do.
- **UDP first, DERP when it must.** A native peer that can take a direct path
  gets one; a browser has no UDP and always goes through DERP. Once a peer's
  handshake completes the machine tells it every UDP endpoint it knows, as
  hole-punching requires.
- With a token beside it both roads run, and the tunnel is then the one road
  the console takes — a shell line never travels through the relay.
  `MON_TRANSPORT=relay` or `=tailcat` forces one road for a run.
- `subnsub-monitor direct on|off`, `allow`, `disallow`, `address` (the
  machine's direct address and node key), `transfer on|off` (whether an
  allowed browser may route one of its own file transfers through this
  machine's tunnel; on by default once direct is on).

The tailcat key and the pre-shared key the address carries live in the
config directory (`tailcat.key`, `tailcat.psk`, 0600) and go with
`--uninstall`. The tunnel adds about 12 MB to the binary and the build needs
Go ≥ 1.27 (`build.sh` fetches the toolchain).

## What it can read, and what each reading costs

Nine built-in providers, a proxy rung, and quota tiles of your own. What each
one *costs to read* is the single most important fact about this program.
There are four cost rungs:

| rung | providers | how | credential held by the helper |
|---|---|---|---|
| **the vendor's own CLI** | Codex (`codex app-server`, JSON-RPC over stdio), Amp (`amp usage`), Kiro (`kiro-cli chat --no-interactive /usage`) | runs the tool, parses what it prints; the request is the tool's, not ours | none |
| **a server on loopback** | Antigravity (the IDE's language server) | finds the server already listening on `127.0.0.1`, reads the `--csrf_token` it was started with, asks it; where the `agy` CLI is installed instead, `agy -p /usage` is asked, on the CLI rung | none; nothing leaves the machine |
| **a credential the tool left on disk** | Claude Code (`~/.claude/.credentials.json`, or the macOS keychain), GitHub Copilot (`~/.config/github-copilot/apps.json`), Kimi (`~/.kimi-code/credentials/kimi-code.json`), Grok (`~/.grok/auth.json`), Kilo (`~/.local/share/kilo/auth.json`) | reads the token that tool stored, asks the vendor that issued it and nobody else, never writes back | yes, in memory, for one request |
| **a CLI Proxy API** | any credential the proxy holds | the proxy's management API, see [CLI Proxy API](#cli-proxy-api) | the proxy's management key only |

Codex's fallback, for a machine with no Codex binary the helper is willing
to run, is the rate-limit object Codex writes into its session logs
(`~/.codex/sessions/**/rollout-*.jsonl`); the dashboard names which of the
two answered and shows the age of a reading that has stopped moving. From
`auth.json` only the id token's e-mail and account id are decoded; the
access and refresh tokens are not parsed.

Every reading carries the figures (percentages, windows, reset times,
amounts, the plan name, credits) and **the account they belong to** — the
e-mail address or login the tool is signed in with — so the dashboard can
show whose quota a tile is and fold one account across machines. Platforms
that only answer to a browser cookie or a pasted key are ones the helper
will never hold a credential for; a command you write can, and
[a quota of your own](#a-quota-of-your-own) puts its answer on the board.

Which provider needs what, and which were considered and left out, is on
the dashboard's coverage card. A machine without a given tool reports it as
`not signed in`, folded into one quiet line.

### Running another program

Amp, Kiro, Antigravity's `agy` and Codex's app-server mean this helper
executes binaries, which the rest of it does not — the rest reads files and
makes one HTTPS request per vendor. The rules that come with that:

- the binary is resolved to an **absolute path from a fixed list** (the
  tool's own install locations plus the usual package prefixes), not taken
  from `PATH`; `PATH` remains a last resort, and `MON_AMP_BIN`, `MON_AGY_BIN`,
  `MON_CODEX_BIN`, `MON_KIRO_BIN` override everything — an override that does
  not check out is a refusal, not a reason to run something else;
- a **world-writable** binary is refused;
- stdin is the null device, there is a deadline, output is read into a
  bounded buffer, and the child gets a filtered environment;
- failures never quote the path being run: on a normal install that path
  carries the local username, and collector failures are published.

## The line that holds

**Reading a credential is not shipping one.**

- The Claude token is read, used for exactly one request to
  `api.anthropic.com`, and dropped. Never logged, never cached, never sent
  anywhere else. Only four scalars are read out of `claudeAiOauth`; the MCP
  tokens in the same file are left alone.
- No credential is **refreshed or written back**. An expired token is
  reported as expired and left for the tool to renew.
- Amp's `secrets.json` is never opened. Each token the helper does hold goes
  only to a vendor host compiled into the binary.
- No error text derived from an exception, a URL or a header is allowed into
  the output — collector failures are *published*, and an HTTP layer quoting
  an `Authorization` header into an error string is exactly the shape that
  must never escape.
- No browser cookies are read. No Full Disk Access is requested. Nothing
  reads your shell history, your environment, your SSH keys or your files.

What does travel, in the open, with the figures: the account name each
reading belongs to; the machine's interface names and addresses; its public
IPv4 (looked up every 30 minutes at `1.1.1.1/cdn-cgi/trace`, `MON_IPV4=off`
stops it); the folded mount paths below. Never: the hostname, the OS
username, process names (those go only on request, see diagnostics), file
contents. The
[threat-model page](https://tools.subnsub.com/monitor-security) lists every
field, every switch, and what the relay keeps of it for how long; the shape
on the wire is the `Snapshot` type in [`go/types.go`](go/types.go), and
`subnsub-monitor` with no arguments prints one.

## Machine health

Alongside quota, the helper reports whether the machine itself is healthy.
Every field here had to clear one bar: **useful for deciding whether a
machine needs attention, useless for working out whose machine it is.** The
kernel version is cut to `major.minor` — "6.8" tells you whether a box is
ancient; "6.8.0-1050-oracle" also tells anyone who reads it which cloud to
aim at.

| | Linux | macOS | FreeBSD | Windows (not published) |
|---|---|---|---|---|
| CPU % and split | user / system / **iowait** / **steal** | user / sys | — | user / system |
| Memory, available / cached | yes | yes | — | yes |
| Swap, pages in / out | yes | **cannot** (`vm.swapusage` does not mean it) | — | **cannot** |
| Root filesystem, other mounts | yes | yes | — | system volume |
| Disk read / write rate | yes | **cannot** (no cgo-free counter) | — | — |
| Network rate, totals, packets / errors / drops | yes | rate and totals | — | — |
| Interfaces and addresses | yes | yes | yes | yes |
| TCP established / TIME_WAIT / retransmits | yes | — | — | — |
| Process count | yes | yes | — | — |
| Temperatures, per sensor | yes (`hwmon`) | yes | — | — |
| Load average | yes | yes | — | **does not exist** |
| Uptime, cores, arch, kernel | yes | yes | partly | yes |
| Host occupancy of memory (virtio balloon) | yes | — | — | — |

The CPU split is there for two of its four numbers: `iowait` is a processor
sitting idle on a disk, and `steal` is time the hypervisor handed to
somebody else — the whole explanation for an oversold VPS that feels slow
while reporting spare capacity. A platform that keeps neither counter omits
the fields rather than sending zeroes.

**Mount points are folded before they are sent**: a path is cut to a few
segments and anything under a home directory, an automount root or the
removable-volume folder collapses to that folder itself
(`/home/alice/projects/big` → `/home`, `/Volumes/Alice Tax Backup` →
`/Volumes`), so the segments an operating system generates from an account
name are dropped. A path you chose the words for (`/srv/customer-acme`) is
sent as you wrote it: it is the answer to "which disk is full". The list
keeps the fullest filesystems rather than the largest.

**Process names are never part of a reading.** On Linux, where `/proc` shows
other processes, the dashboard may ask for the twenty busiest (pid, name,
CPU, memory, age) — read-only, no command lines, environment, working
directories or open files — and the helper keeps the five busiest program
names beside each bucket of its own history so the CPU chart can say whose
CPU it was; that history is served over the direct tunnel only. This is not
a switch (`subnsub-monitor diagnostics` only reports).

macOS reads most of its health through Apple's own command-line tools —
`sysctl`, `vm_stat`, `top`, `ps`, `netstat`, each on the read-only system
volume by absolute path, deadlined, and parsed by something that refuses
every line it cannot fully account for — because without cgo the Go standard
library cannot read the sysctls that would answer in process. FreeBSD reports
its platform, cores and interfaces and names everything else as missing.
Anything a platform cannot measure is named in a `missing` list so the
display can say so rather than render a zero that looks like an idle
machine.

The machine also records on its own what the relay could not see: the
stretches it was down, suspended or wiped (a VM the platform re-provisions is
not a reboot; `host-id` tells the two apart), and carries them to the relay
with its next push.

## One machine, one dashboard

Every machine you paste the same token on gets its own card, told apart by
an `agent_id` created on first run and kept in
`~/.config/subnsub-monitor/agent-id`. **It is random, not derived** — no
hostname, no MAC, no machine-id, nothing hashed from any of them: an id
derived from hardware is a fingerprint that survives reinstalls. Delete the
file and you get a new one; set `MON_AGENT_ID` to pin it, which is what a
container image wants.

A name is optional and is never inferred — in particular never from the
hostname:

```sh
sh install.sh <TOKEN> --name "tokyo build box"   # at install time
subnsub-monitor name "tokyo build box"           # any time after
subnsub-monitor name                             # show what it will report
```

Control characters, bidi overrides and invisible padding are stripped before
it travels, and it is cut to 24 characters. A rename from the dashboard is a
signed command that writes the machine's `name` file; the running helper
reads it without a restart.

## The console, and why it is off

The dashboard can run a command on a machine and show you the output. **This
is off unless you turn it on here, on the machine**, and that is the design
rather than a default:

```sh
sh install.sh <TOKEN> --console     # at install time
subnsub-monitor console on          # any time after
subnsub-monitor console off         # and back off — within one poll, no restart
```

The switch is the existence of `~/.config/subnsub-monitor/console`, written by
someone who already had a shell here (`MON_CONSOLE=1` overrides it for one
run). With it on, a line runs as `/bin/sh -c <line>` as this user, in this
user's home directory; each command is its own process, 30 seconds, output
capped at 16 KB, the process group taken down when it finishes, and **every
command is written to this machine's own log before it runs** — the journal
on Linux, the launchd log on macOS.

Where a command may come from is the part that matters:

- **A shell line travels only the direct tunnel.** The relay's mailbox
  refuses one outright, whatever it carries and whoever signed it
  (`console.go`, `mailboxRefusal`). So a machine with the console on and no
  tunnel takes no shell line at all.
- **The sealed road** is the one exception, for a machine that can only dial
  out and cannot hold a tunnel: a line sealed in the browser to this
  machine's own X25519 key (X25519, HKDF-SHA256, AES-256-GCM, the command id
  as associated data) rides the relay's mailbox as ciphertext the relay
  cannot open, and the output goes back sealed to the browser that signed it
  (`econ.go`). It needs the console on *and* a pinned key, and the dashboard
  turns it on per machine.
- **A terminal** — a real pty, the same thing the dashboard's `>_` opens —
  is tunnel-only as well (`pty.go`).

On Linux the systemd unit's confinement follows this setting: without the
console the service runs under `ProtectSystem=strict` and a read-only home;
with it on, the ordinary confinement for a user service. `NoNewPrivileges`
holds either way, and the installer prints which it used.

### Who may command this machine

Turning the console on says *whether* this machine takes instruction. **Who**
is a separate question, answered by signatures (`signing.go`):

```sh
subnsub-monitor trust                 # list the pinned dashboard keys
subnsub-monitor trust <KEY>           # add one (a second browser, a colleague)
subnsub-monitor untrust <KEY>         # remove one
```

The install line copied from a signed-in dashboard pins that account's
public key (`MON_PUBKEY` → `trusted-keys`, 0600). **There is deliberately no
network path that adds a key.** Once anything is pinned, a command must
carry an Ed25519 signature made by a pinned key — the private half lives in
the browser and never travels in a command — it is good for five minutes
either side of its timestamp, and its id is accepted once. A relay can
therefore drop, delay or withhold a command, but it cannot compose one or
replay one. Shell lines, sealed lines, tunnel commands, renames, tile presses
and the enrolment of portable copies all require a pinned key; with nothing
pinned, the only commands the relay can hand this machine unsigned are the
closed forms — check for an update (if allowed), refresh the quota, list
processes, look up the public IP — and the dashboard's card says which state
a machine is in, because they are not equally safe.

### A portable copy

A dashboard can lend its command line to a computer's command line for a
while (Monitor page: a machine's terminal → Create a copy; 1 hour, 1 day or
7 days). On that computer:

```sh
pbpaste | subnsub-monitor clone use          # keep the copied line
subnsub-monitor run <machine> -- <command>   # one console line there; output and exit status come back
subnsub-monitor shell <machine>              # a terminal
subnsub-monitor clone                        # what it covers, until when
subnsub-monitor clone forget                 # delete it
```

A copy is a new pair of keys (Ed25519 for signing, a tailcat node key) and a
certificate the dashboard signs with its own command key, naming the machines
it covers and when it expires (`clone.go`, `mon-clone-v1`). Each machine
admits it for console lines and terminals only, over tailcat, while the
certificate is in date, within scope and signed by a key still pinned there;
expiry is judged by the machine's own clock, seven days at most. Revoke from
the dashboard's list, or on the machine with `subnsub-monitor clones revoke
<id>`. **For as long as it is valid it is a shell on that machine**, with
everything this user can do; treat the copied line accordingly.

## Updating itself, and who decides what that means

A new release should not mean logging into every machine. The dashboard can
ask a helper to replace its own binary, and the shape of that is one
sentence:

> **The dashboard decides WHEN. The release bucket decides WHAT.**

The message the dashboard sends carries a machine id and a correlation id and
**nothing else** — no field could name a version, a URL or a binary. The base
URL is a constant in [`go/update.go`](go/update.go); only the local
`MON_UPDATE_BASE` moves it, and redirects are refused. What happens, in
order, stopping at the first failure: read the published version and refuse
anything that is not `YYYY.MM.DD.N`; refuse to go **backwards**; download
(64 MiB cap) and check the SHA-256 against the published `SHA256SUMS`; **run
the new binary** and require it to agree about its own version; hard-link the
running binary aside and replace it with one atomic rename; report, then exit
and let `systemd`/`launchd` start the new one. The previous binary stays as
`<binary>.prev`.

```sh
sh install.sh <TOKEN> --remote-update   # at install time
subnsub-monitor update on|off           # any time after; --console implies on
subnsub-monitor autoupdate on|off       # hourly, unasked; counts only while update is on
```

The reading says what the timer is doing — when the next pass is due, how the
last one ended, and `installing` from the moment a download starts — so a
card can say "next check in 23 min" instead of leaving a behind machine
looking stuck. On a Linux box with other named network namespaces (a VPN
exit on a router), `subnsub-monitor roads` measures each way out against the
release host and the update is fetched through the fastest; every five
minutes the helper also makes one request to the relay from inside each
namespace so the card can show that exit's address (`MON_NETNS=off`, or
`MON_NETNS=a,b` to limit it; needs the privilege to enter a namespace).

The integrity of an update rests on TLS to the release host plus that host's
checksum list; there is no separate signing key on the binary yet. The
adapter table the helper fetches *is* Ed25519-signed against a key compiled
into it (below). Turning update on is trusting the release host for code the
same way installing did on day one.

## Coding-agent sessions

The dashboard can search a machine's coding-agent sessions — Claude Code,
Codex, Cursor, Grok, Kiro and Amp transcripts, read right where the tools
left them. The search is answered by the helper over the console channel, so
it reaches a machine only with the console on, and only matching snippets
and the pages you open leave it. The same on the machine:

```sh
subnsub-monitor sessions search <text>
subnsub-monitor sessions recent
subnsub-monitor sessions open --id <id>
```

A machine that is asleep, or gone, cannot answer. **Sessions sync** keeps
them: with `--sessions-sync` at install or `subnsub-monitor sessions sync on`
(`MON_SYNC` for one run), the helper parses its own transcripts, redacts
them, and pushes the message text — each message cut to 4000 characters, with
the title, project name, timestamps and counts — to your account's archive
over the connection it already has. Off until you turn it on, at the
machine; with the switch absent the helper never reads a transcript for this
purpose. Redaction is a best-effort pass over common token shapes, JWTs,
private keys and `secret=`-style assignments, not a guarantee: turn this on
only if you want the transcripts searchable from the dashboard and are
content for them to sit on the relay (180 days after the last chunk, and
not deleted by uninstalling — the dashboard's Drop does that).
`subnsub-monitor sessions sync --dry-run` shows what would go without opening
a connection.

## Service checks

The helper probes your own services from the machine it runs on. Targets are
listed in `~/.config/subnsub-monitor/probes.json` (owner-only, beside the
token); the helper re-reads the file whenever it changes, so there is nothing
to reinstall or restart.

```json
{
  "name": "Home services",
  "probes": [
    {"name": "Blog", "type": "http", "url": "https://blog.example.com"},
    {"name": "API", "type": "http", "url": "https://api.example.com/health",
     "expect": [200, 204], "keyword": "\"ok\"", "interval": 30, "retries": 2},
    {"name": "Postgres", "type": "tcp", "host": "10.0.0.5", "port": 5432},
    {"name": "Zone", "type": "dns", "host": "example.com", "tags": {"env": "prod"}}
  ]
}
```

A bare array of probes is accepted too. Per probe:

| field | meaning |
|---|---|
| `type` | `http`, `tcp` or `dns` |
| `url` | http: `http://` or `https://` with a host and no credentials |
| `host`, `port` | tcp: what to connect to; dns: the name to resolve (no port) |
| `name` | the label the dashboard shows; defaults to the id |
| `id` | optional, `[A-Za-z0-9_-]{1,48}`; derived from the target when absent (an HMAC under a random key kept in `probes.key`, so the id is stable across renames and restarts but says nothing about the target) |
| `interval` | seconds between checks, 15–3600, default 60 |
| `timeout` | seconds per check, 1–60, default 10 |
| `retries` | failed checks tolerated before **down** (shown as **pending** meanwhile), default 0 |
| `maintenance` | `true` parks the probe: reported as maintenance, never checked |
| `expect` | http: acceptable status codes, default 200–299 |
| `keyword` | http: text the body must contain (first 1 MiB) |
| `method` | http: `GET` (default) or `HEAD` |
| `insecure` | http: skip TLS verification; the certificate is then judged by its dates alone |
| `tags` | free labels shown on the card; keys that look like a target or a credential are dropped |

At most 64 probes per machine. `subnsub-monitor probes` validates the file
and prints one round of results with their reasons; `--json` prints exactly
what the dashboard would receive.

Monitor receives only the probe's name, type, state and tags; the last
successful check's response time; 24-hour, 30-day and 365-day uptime,
computed on the machine from hourly and daily buckets in `probes-state.json`;
and TLS validity and remaining certificate days for https targets. URLs,
hostnames, ports and the reason a check failed never leave the machine. A
file the helper cannot read keeps the last good set running and marks the
source as needing attention on the dashboard.

## Your own tiles

Everything else the helper reports it decided to measure. This is the other
direction: any number, state or word you want on the machine's card — a GPU
temperature, a UPS charge, last night's backup, a solar yield, the build that
is live — drawn as the same tiles the System block uses.

The fastest way is one command on the machine:

```sh
subnsub-monitor metric set gpu_temp 71 --name GPU --type gauge --unit celsius --warn 80 --bad 90 --icon gpu --history
subnsub-monitor metric set ups 96 --type gauge --unit percent --invert --warn 40 --bad 15 --icon battery
subnsub-monitor metric set backup ok --label "02:14 · 38 GB" --icon shield
subnsub-monitor metric set solar 12.4 --suffix kWh --icon sun --history
subnsub-monitor metric set build v2026.09.17.2 --type text --icon tag --tint muted
subnsub-monitor metric ls
subnsub-monitor metric rm build
```

`set` upserts one tile by id; fields you do not name keep what they had, so a
cron job that only runs `metric set solar 13.1` every minute leaves the icon
and the unit alone. The dashboard shows the change within a push.

Under the hood `metric set` edits one file in a directory, and you can write
files there yourself — from any language, on any schedule:

```
~/.config/subnsub-monitor/metrics.d/<source>.json
```

Each file is a **source** (its stem is the id the dashboard groups errors
under; letters, digits, `-` and `_`) and holds tiles:

```json
{
  "name": "Home lab",
  "tiles": [
    {"id": "gpu_temp", "name": "GPU", "type": "gauge", "value": 71,
     "unit": "celsius", "max": 100, "warn": 80, "bad": 90, "icon": "gpu",
     "history": true, "ttl": 120},
    {"id": "ups", "name": "UPS", "type": "gauge", "value": 96, "unit": "percent",
     "invert": true, "warn": 40, "bad": 15, "icon": "battery", "group": "Power"},
    {"id": "backup", "name": "Nightly backup", "type": "status",
     "value": "ok", "label": "02:14 · 38 GB", "icon": "shield"},
    {"id": "solar", "name": "Solar today", "type": "number", "value": 12.4,
     "suffix": "kWh", "icon": "sun", "history": true},
    {"id": "build", "name": "Prod build", "type": "text", "value": "v2026.09.17.2",
     "icon": "tag", "tint": "muted", "group": "Deploy"},
    {"id": "fan", "name": "Fan", "type": "number", "unit": "rpm", "icon": "fan",
     "exec": "sensors | awk '/fan1/{print $2}'", "interval": 30}
  ]
}
```

A bare array of tiles is accepted too. Write the file whole (write a
temporary name and rename it over the old one); the helper re-reads it when
it changes. Per tile:

| field | meaning |
|---|---|
| `id` | `[A-Za-z0-9_-]{1,48}`; derived from `name` when absent. The trend hangs off it, so keep it stable |
| `name` | the label the dashboard shows; defaults to the id |
| `group` | the section head the tile sits under on the card; tiles without one share a section |
| `type` | `gauge`, `number`, `status`, `text` or `quota`. Defaults to `number` for a numeric value, `status` for a bare state word, `text` otherwise |
| `value` | gauge/number: a number (`"71.5%"` and `"1,200"` are read as numbers too). status: `ok`, `warn`, `bad`, `pending` or `unknown` (`up`/`down`/`fail`… are folded onto these). text: the string |
| `label` | status: a few words beside the state |
| `min`, `max` | gauge: the track, default 0–100. The meter fills to the value's place on it |
| `unit` | how the dashboard formats the number: `percent` `bytes` `bytes_s` `bits_s` `ms` `s` `celsius` `count` `usd` `credits` `watts` `rpm` `hz` |
| `suffix` | a unit the list lacks (`kWh`, `ppm`), up to 8 characters, printed after the plain number. One of `unit`/`suffix`, not both |
| `warn`, `bad` | thresholds that colour the value and mark the card hot. A gauge defaults to 75 % / 90 % of its track; `"alerts": false` turns that off |
| `invert` | `true` when lower is worse (a battery, a hit rate): thresholds flip and the trend keeps the dip |
| `icon` | a name from the table below, or `emoji:🔋` for one emoji. Anything else draws the name's initial |
| `tint` | colour for a number or text value: `accent` `ok` `warn` `bad` `muted` `blue` `violet` `pink` `orange` `teal` `green` `red`. Gauges and statuses take their colour from the thresholds instead |
| `at` | unix seconds the value was measured; defaults to the file's mtime |
| `ttl` | seconds after `at` before the tile dims as **stale**. A tile with neither `at` nor `ttl` nor `exec` is a declaration and never goes stale |
| `history` | `true` keeps a two-day trend behind a press on the tile, like CPU or a filesystem. A `status` keeps the worst state of each five minutes |
| `fold` | `max` (default) or `min`: which extreme a five-minute history bucket keeps. Inverted gauges default to `min` |
| `tags` | up to 4 free labels shown in the tile's tail |
| `exec` | a command the helper runs for the value — a string through `/bin/sh -c` (`cmd /c` on Windows) or an argv array. See below |
| `interval` | exec: seconds between runs, 5–3600, default 60 |
| `timeout` | exec: seconds per run, 1–60, default 10 |

At most 32 tiles per machine across at most 8 files. `subnsub-monitor metrics`
on the machine reads every file, says what is wrong with a broken one, runs
each command once and prints every tile; `--json` prints exactly what the
dashboard would receive.

### Commands

A tile with `exec` is collected by the helper on its own timer, as the user
the helper runs as, from your home directory. What the command prints is:

- **a bare value** — the number (`42`, `71.5%`, `1200 rpm`), the state word
  with an optional label (`ok`, `DOWN since 02:14`), or the text. Only the
  first line counts, so a chatty command still works;
- **a JSON object** of tile fields, laid over the entry (`{"value": 71,
  "label": "…", "name": "…"}`) — everything but the id may move;
- **a JSON array** of tiles, each inheriting the entry's `group`, `unit`,
  `icon`, thresholds and so on unless it sets its own — one command producing
  a whole group:

```sh
#!/bin/sh
# gpus.sh — one gauge per GPU, from one call
nvidia-smi --query-gpu=index,temperature.gpu --format=csv,noheader,nounits |
  awk -F', ' 'BEGIN{printf "["} NR>1{printf ","} {printf "{\"id\":\"gpu%s\",\"name\":\"GPU %s\",\"value\":%s}", $1, $1, $2} END{print "]"}'
```

```json
{"id": "gpus", "exec": ["/home/me/gpus.sh"], "type": "gauge", "unit": "celsius",
 "group": "GPUs", "warn": 80, "bad": 90, "icon": "gpu", "history": true, "interval": 30}
```

A command that fails, times out or prints something that is not a value keeps
the tile on the card with its last good value, dimmed, with the reason in its
tail (`command failed`, `command timed out`, `command printed no value`); what
it actually printed is in the helper's log and in `subnsub-monitor metrics`.
A tile whose command has not succeeded for three intervals is shown stale.

### Actions

A tile can also be pressed. An entry names `actions`: commands a press on
the dashboard runs, written here beside the tile, where they can be read —
the dashboard never sends a command, only which action and, for one that
takes it, a number inside the bounds declared with it:

```json
{"id": "weights", "exec": ["/home/me/weights.sh", "show"], "interval": 15, "group": "Weights",
 "actions": [
   {"id": "set", "label": "Set", "exec": ["/home/me/weights.sh", "set"],
    "input": {"min": 0, "max": 20, "step": 1}},
   {"id": "reset", "label": "Reset", "exec": ["/home/me/weights.sh", "reset"], "confirm": true}
 ]}
```

| field | meaning |
|---|---|
| `id` | `[a-z][a-z0-9_-]{0,23}`, unique on the tile; four actions at most |
| `label` | the press's words, 24 characters at most; defaults to the id |
| `exec` | the command, a string through `/bin/sh -c` or an argv array |
| `input` | `{"min", "max", "step"}`: the action takes a number on that grid (step defaults to 1). Without it the action takes nothing |
| `confirm` | `true` arms the press first; a second press within four seconds runs it |
| `tone` | `bad` for an action that takes something away |
| `timeout` | seconds, 1–60, default 20 |

The command learns what was pressed from its environment, never from its
command line: `SUBNSUB_TILE` (the pressed tile's id), `SUBNSUB_ACTION`,
`SUBNSUB_VALUE` (the number, canonical, or empty) and `SUBNSUB_SOURCE` (the
file's stem). Its exit code is the outcome; the first 400 characters of what
it printed — of stderr when it fails — are shown under the press. The tile is
read again at once, so the card shows what the press did.

Every tile an entry's command prints offers that entry's actions; a printed
tile may narrow them with `"actions": ["set"]` (ids only — a printed tile can
never declare one, and neither can a tile pushed over the socket).

Only a browser this machine has pinned (`trusted-keys`) can press, over the
direct tunnel or as a signed command through the relay's mailbox; a machine
with nothing pinned takes no press at all, and a portable copy is not granted
them. Each press is written to the helper's log.

### Icons

Built in, drawn in the same stroke as the System tiles:

`gpu` `thermometer` `fan` `battery` `plug` `bolt` `database` `container` `cloud`
`clock` `calendar` `timer` `dollar` `users` `mail` `shield` `lock` `camera` `home`
`bulb` `sun` `moon` `leaf` `wind` `snow` `gear` `chart` `check` `warning` `bell`
`file` `printer` `globe` `heart` `star` `download` `upload` `percent` `hash`
`terminal` `code` `git` `rocket` `car` `video` `phone` `radio` `pin` `coffee`
`game` `music`

and the System block's own: `cpu` `memory` `disk` `server` `storage` `network`
`wifi` `swap` `gauge` `pulse` `drop` `key` `tag` `list` `flag` `city` `office`
`palette`. Common synonyms map onto these (`temperature`, `power`, `money`,
`alert`, `settings`, `branch`, `location`, `docker`, …). `emoji:` followed by
one emoji draws that emoji. A name the dashboard does not know draws the
tile's initial in a box rather than nothing.

### Pushing from a running process

A daemon that reads a sensor every second should not rewrite a file every
second. The running helper listens on a socket only its own user can reach,
`~/.config/subnsub-monitor/metrics.sock`: one JSON value per line — a tile or
an array of tiles, the same fields as in a file — and one line back per line
sent, `ok 2` or `error: …`.

```sh
subnsub-monitor metric push '{"id":"bench","name":"Bench","type":"gauge","value":41.5,"unit":"celsius"}'
subnsub-monitor metric push '{"id":"bench","value":42.1}'     # later pushes name only what moved
subnsub-monitor metric push '{"id":"bench","remove":true}'
my-daemon | subnsub-monitor metric push                          # lines on stdin, one connection
```

```python
import json, socket, os
s = socket.socket(socket.AF_UNIX); s.connect(os.path.expanduser("~/.config/subnsub-monitor/metrics.sock"))
s.sendall((json.dumps({"id": "bench", "value": 42.1}) + "\n").encode()); print(s.recv(4096).decode())
```

A push to an id seen before is laid over what was pushed earlier, so the
shape goes once and then only the value. `at` is the moment of arrival and
`ttl` defaults to five minutes: a daemon that dies leaves a stale tile, and
one silent for a day is forgotten. Nothing is written to disk, so a helper
restart starts empty. `exec` is refused on the socket — commands live in
`metrics.d/`, where they can be read. Pushed tiles sit under the source
`push`, after the files' tiles (a file naming the same id wins), share the
32-tile and 8-trend limits, and show up in `subnsub-monitor metrics`. A line
is at most 64 KB; eight connections at a time; an idle connection is closed
after ten minutes.

One line on the socket is not a tile. A system that knows something about
this machine's downtime that the machine could not see for itself — the
service that wiped and rebuilt it, the platform that paused it — files it as
an outage with the kind it knows:

```sh
subnsub-monitor metric push '{"declare":{"kind":"wipe","at":1790371049,"end":1790371965,"by":"restore-service"}}'
# → ok declared wipe 1790371049-1790371965
```

`kind` is `wipe`, `down` or `suspend`; `at`/`end` are unix seconds (default:
the helper's own alive-stamp and now); `by`, `confirmed_at` and `note` go to
the log. The same object left in the state directory as `declared-outage`
before the helper starts is read once at boot and consumed.

### A quota of your own

The collectors read what the machine holds: the vendor's credential files,
its CLI, a server on loopback, a CLI Proxy API. A platform that only answers
to a browser cookie or a key you paste — OpenCode's Zen page, Cursor — is one
the helper will never hold a credential for. A command you write can, and
its answer belongs on the quota board, not among the gauges. So a tile of
type `quota` is drawn as an agent: your name where the vendor's goes, a
monogram where the vendor's mark goes, the same rows, reset times and
balance the built-in agents get, and "your own command" where theirs say
"live query".

```json
{"tiles": [
  {"id": "opencode", "name": "OpenCode", "type": "quota",
   "exec": "sh ~/.config/subnsub-monitor/opencode-quota.sh", "interval": 300, "timeout": 20}
]}
```

The command prints a JSON object, or a bare number (one window, `usage`,
at that share):

```json
{"plan": "Zen Pro",
 "windows": [
   {"key": "monthly", "label": "30d", "used": 7.5, "total": 20, "unit": "usd",
    "minutes": 43200, "resets_at": "2026-11-01T00:00:00Z"},
   {"key": "weekly", "used_percent": 62}
 ],
 "balance": "12.50"}
```

| Field | |
|---|---|
| `plan` | the plan's name as the vendor says it (40 at most) |
| `windows[]` | one per limit, 8 at most |
| `windows[].key` | `[a-z0-9_-]{1,24}`, the row's key; made from `label` when absent, `usage` when both are |
| `windows[].label` | words for the row (`30d`, `monthly`); defaults to the key |
| `windows[].used_percent` | 0…100, up to 1000 for an allowance run past (drawn as "312.7% over" on a hatched track); worked out from `used` / `total` (or `remaining` / `total`) when absent |
| `windows[].used`, `total`, `remaining` | the figures behind the share, in `unit`: `usd` or `credits`; `count` (requests, rows — written short, 2.8M) or `bytes` (decimal units, 41.3 GB) |
| `windows[].cost` | what the window has cost this period, in US dollars — an allowance's overage, say; shown after its figures |
| `windows[].minutes` | the window's length |
| `windows[].resets_at` | unix seconds or an ISO-8601 time; a time already past is not a reset |
| `balance` | a prepaid balance, number or string; `"unlimited": true` for a plan without one |
| `error` | what the command found instead of a reading (`"not signed in"`): the agent shows as failed, with those words |

A quota entry takes `id` (16 characters at most), `name`, `icon`, `exec`,
`interval`, `timeout`, `at` and `ttl` like any tile, and none of a gauge's
fields. `error` and a reading are exclusive; the newest answer wins. The
same object goes over the socket (`metric push`) from a process that already
has the numbers.

A bill reads the same way: a window keyed `bill` in `usd` — the period's bill
against a budget of your own — is what the folded status bar shows for the
agent (as an amount, `$5.52`), instead of the share of its fullest window.
`metrics.d.examples/cloudflare.json` is a ready-made one for Cloudflare
Workers Paid.

On the wire the agent is `custom-<id>` — your entry called `claude` does not
wear Claude's mark or links — with source `custom` and collector `operator`.
The relay rebuilds it field by field like any provider; a reader of the board
can see it is yours, which is exactly how far the number can be trusted.

### Ready-made files

[`metrics.d.examples/`](metrics.d.examples/) holds files to copy as they are:
NVIDIA GPUs, lm-sensors, a NUT UPS, Docker, failed systemd units, SMART, ZFS,
Pi-hole, latency, and the Cloudflare bill. Each is one `exec` entry (or a
few) and needs only the tool it reads. `quota-demo.json` is the shape of a
quota of your own, with a command that prints fixed numbers — the place to
start a reader of your own from.

Monitor receives the fields above and nothing else: no command line, no file
path, no output beyond the value. The relay rebuilds every tile field by
field, so a field this list does not name does not reach a browser.

## CLI Proxy API

If the machine runs [CLI Proxy API](https://github.com/router-for-me/CLIProxyAPI)
(the proxy behind the CLI Proxy API Management Center), the helper can read
the quota of every credential the proxy holds — Claude, Codex, Antigravity,
Kimi and Grok, one tile per account — through the proxy's own management API,
the way the Management Center's quota page does. The credential never leaves
the proxy; the helper only holds the proxy's management key.

```sh
export MON_CPA_URL='http://127.0.0.1:8317'   # optional; this is the default
export MON_CPA_KEY='<MANAGEMENT_KEY>'         # remote-management.secret-key

curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- '<MONITOR_TOKEN>'
```

A vendor the proxy holds a credential for is read through the proxy, and the
helper's own collector for that vendor stands down; a vendor the proxy knows
nothing about keeps its native collector. `MON_CPA_URL` may be plain HTTP
only on loopback; anywhere else it must be HTTPS. The key is stored 0600
beside the monitor token and is sent to nothing but that address. The helper
calls exactly two management endpoints: `GET /v0/management/auth-files` (the
list of credentials, minus anything secret) and
`POST /v0/management/api-call` (a vendor request the proxy signs). It never
downloads a credential file.

The vendor endpoints, headers and field names this rung reads are a table,
[`go/adapters.json`](go/adapters.json), not code. A newer table reaches every
helper in the field without a build: the daemon fetches `adapters.json` +
`adapters.sig` from the release host a few times a day, accepts a table whose
`version` is above the one it holds and whose Ed25519 signature checks
against the key compiled into it (`adapterFeedPubKey` in `adapterfeed.go`),
and reads by it from the next push on. The last good table is kept beside
the token and re-verified at start. `MON_ADAPTERS=off` keeps the embedded
table. A table can name fields and formats; it cannot name a host outside the
allow-list in `cpa.go`, and it never carries a credential.

## Every command

```
subnsub-monitor                        one snapshot as JSON — exactly what the relay would receive
subnsub-monitor watch [SEC]            reprint every SEC seconds
subnsub-monitor serve [PORT]           serve /quota + /events on 127.0.0.1 only
subnsub-monitor connect URL [TOKEN]    dial out and push to the relay (what the service runs)
subnsub-monitor unregister URL         tell the relay this machine is gone
subnsub-monitor name [LABEL]           show or set this machine's dashboard name
subnsub-monitor console [on|off]       may the dashboard run commands here?
subnsub-monitor update [on|off]        may the dashboard replace this helper?
subnsub-monitor autoupdate [on|off]    does the helper replace itself, hourly, unasked?
subnsub-monitor diagnostics            report whether program names are read here (always, where /proc allows)
subnsub-monitor autostart              OpenWrt: arrange (or report) starting at boot
subnsub-monitor spaces [RELAY]         probe the other network namespaces' exits once and print
subnsub-monitor netns                  how many network namespaces this machine shows, and which one the helper runs in
subnsub-monitor roads                  measure every way out against the release host once and print
subnsub-monitor probes [--json]        check the services in probes.json once and print each verdict
subnsub-monitor metrics [--json]       read metrics.d/, run each command once and print every tile
subnsub-monitor metric set ID VALUE    put your own number on this machine's card (metric rm / ls / push too)
subnsub-monitor temps                  print this machine's temperature sensors as JSON
subnsub-monitor sessions search TEXT   search this machine's coding-agent sessions
subnsub-monitor sessions recent        list the newest coding-agent sessions here
subnsub-monitor sessions open --id ID  print one session's messages, a page at a time
subnsub-monitor sessions sync [on|off] do sessions go to the account archive? (--dry-run: what would go)
subnsub-monitor trust [KEY]            list, or add, a dashboard key allowed to command this machine
subnsub-monitor untrust KEY            stop accepting commands signed by that key
subnsub-monitor direct [on|off]        may a browser reach this machine directly?
subnsub-monitor allow [KEY]            list, or add, a browser key allowed through the direct tunnel
subnsub-monitor disallow KEY           stop serving that browser key
subnsub-monitor address                print this machine's direct address and node key
subnsub-monitor transfer [on|off]      may an allowed browser route a file transfer through this tunnel?
subnsub-monitor clone use|forget       keep, or delete, a portable copy of a dashboard's access
subnsub-monitor run MACHINE -- CMD     run one console line on a machine the copy covers
subnsub-monitor shell MACHINE          a terminal on it
subnsub-monitor clones [revoke ID]     the copies this machine has admitted; revoke one
subnsub-monitor version                print this build's version and exit
subnsub-monitor selftest               show what the collectors can and cannot open
```

`serve` only ever shows you *this* machine and binds to `127.0.0.1`, never
`0.0.0.0`. The token may come from `SUBNSUB_MONITOR_TOKEN` or from the file
the installer wrote; both keep it out of `ps`.

## Environment

Each switch is also a file in `~/.config/subnsub-monitor/`; the environment
wins for one run, which is what a container or a test wants.

| variable | |
|---|---|
| `SUBNSUB_MONITOR_TOKEN` | the relay token (otherwise the installer's `token` file) |
| `SUBNSUB_MONITOR_SITE` | an HTTPS site of your own that renews tokens; renewal otherwise happens only against ours |
| `MON_NAME` | the card's name (the `name` file, set from the dashboard or `name`, takes precedence) |
| `MON_AGENT_ID` | pin the machine id instead of the random one in `agent-id` |
| `MON_CONSOLE` | `1`/`0`: may the dashboard run commands here (the `console` file) |
| `MON_UPDATE`, `MON_AUTOUPDATE` | the update and autoupdate switches (the `update`, `autoupdate` files) |
| `MON_UPDATE_BASE` | where releases are fetched from; otherwise a constant in `update.go` |
| `MON_ADAPTERS` | `off` keeps the embedded CPA adapter table and never fetches one |
| `MON_SYNC` | `1`/`0`: sessions sync (the `sessions-sync` file) |
| `MON_DIRECT` | `1`/`0`: direct mode (the `direct` file) |
| `MON_DERPMAP` | an HTTPS URL of a DERP map of your own (the `derpmap` file) |
| `MON_TRANSPORT` | `relay` or `tailcat` forces one road for a run |
| `MON_TRANSFER` | `1`/`0`: file transfers through this machine's tunnel |
| `MON_BEACON` | `off` keeps the full reading going to the relay while a tunnel is up, instead of a beacon |
| `MON_IPV4` | `off`, or an HTTPS URL answering like `cdn-cgi/trace`: the public-IPv4 lookup |
| `MON_MYIP` | `off`, or an HTTPS origin: where the Exit → My IP request goes |
| `MON_NETNS` | `off`, or `a,b`: which named network namespaces to probe (Linux, root) |
| `MON_ROADS` | `off`: skip the exit-speed measurement that picks the road for updates |
| `MON_CLIS` | `off`: do not report which AI CLIs are installed and their versions |
| `MON_CPA_URL`, `MON_CPA_KEY` | the CLI Proxy API's address and management key |
| `MON_COPILOT_TOKEN`, `MON_GROK_TOKEN`, `MON_KILO_TOKEN`, `MON_KIMI_TOKEN` | a token in place of the one the tool left on disk |
| `MON_AMP_BIN`, `MON_AGY_BIN`, `MON_CODEX_BIN`, `MON_KIRO_BIN` | an absolute path to that tool, for an unusual install |
| `MON_AUTOSTART` | `off`: OpenWrt, never write or keep the boot entry |
| `MON_DNS` | Android: resolver addresses where the system's daemon is refused |
| `MON_PUBKEY`, `MON_RELAY`, `MON_BASE`, `MON_BINDIR` | installer only: the key to pin, the relay, the release host, the install directory |

## What is on the machine

Everything the helper keeps lives in `~/.config/subnsub-monitor/` (0700),
owner-only:

| file | |
|---|---|
| `token`, `token.current`, `service.env` | the relay token, the renewed one, the service's environment |
| `agent-id`, `name`, `host-id` | the random machine id, your label, the OS install's identity (never sent) |
| `console`, `update`, `autoupdate`, `direct`, `sessions-sync`, `derpmap` | the switches: existence is the setting |
| `trusted-keys`, `econ-key`, `allowed-clients`, `tailcat.key`, `tailcat.psk`, `address` | who may command; this machine's sealing key; who may watch; the tunnel's keys and address |
| `clone-access`, `clones`, `clones.revoked` | a portable copy this computer holds; the copies this machine has admitted, and those it has revoked |
| `probes.json`, `probes.key`, `probes-state.json` | service checks and their uptime buckets |
| `metrics.d/`, `metrics.sock` | your tiles, and the socket a process pushes them on |
| `history.jsonl`, `outages.json`, `declared-outage` | the machine's own 30-day history and the stretches it was down |
| `adapters.json`, `adapters.sig` | the last good CPA adapter table and its signature (the proxy's address and key live in the token file, as the installer wrote them) |

## Build it yourself

```sh
cd go && sh build.sh          # → dist/subnsub-monitor-<goos>-<goarch>, SHA256SUMS, VERSION
go test ./...
```

Builds are reproducible: `CGO_ENABLED=0`, `-trimpath` keeps absolute paths
out and `-buildvcs=false` keeps git state out. Build the same source anywhere
and you get the same bytes as the published release — check them against the
`SUM_*` lines in `install.sh`:

```sh
cd go && sh build.sh && sha256sum dist/*
```

That is what makes "verify it against the source" a real offer: without the
second flag the same source built from a checkout, a tarball and our own tree
produced three different binaries, and anyone taking the invitation to verify
would have found a mismatch with no way to tell an innocent stamp from a
swapped binary.

`go test ./...` passes here; the tests that compare the helper against the
hosted relay's field tables skip outside the monorepo and say so.

## Reference implementation

[`reference/monitor.py`](reference/monitor.py) is the Python reading of the
quota collectors as they stood when the Go helper was written — one file, no
dependencies, the same `Snapshot` shape — kept as a readable second opinion
on what each provider's data looks like. It reads no `metrics.d/`, probes
nothing, and opens no tunnel; the Go helper is the product.

## License

Apache-2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE); the tunnel's
data plane (tailcat) is BSD-3-Clause and is listed there.
