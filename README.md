# subnsub-monitor

Reads how much AI coding quota is left on a machine, and pushes it somewhere
you can see it.

The point is the machines you **cannot reach**: a VPS behind a cloud firewall,
a box behind NAT, a laptop that moves between networks. Nothing here listens
on a port. The agent dials out over ordinary HTTPS and pushes readings to a
relay; a browser subscribes to the relay. Neither end ever needs an inbound
route.

```
  machine you can't reach                     you
  ┌──────────────────┐                  ┌───────────┐
  │  subnsub-monitor     │──POST /push──▶ relay ◀──WS──│  browser  │
  └──────────────────┘   (outbound only)           └───────────┘
```

## Credit where it is due

The idea — read the quota your AI coding tool has left on this machine, and
put it in front of you — comes from **[CodexBar](https://github.com/steipete/CodexBar)**
by [Peter Steinberger](https://github.com/steipete) (MIT).

CodexBar is a macOS menu-bar app. This is an independent implementation in a
different shape (a headless outbound-push agent), and contains no CodexBar
code — but the idea got here from there, and that is the part worth crediting.

## What it can read, and what each reading costs

Four providers, and what each one *costs to read* is the single most important
fact about this program:

| | Codex | Antigravity | Amp | Claude Code |
|---|---|---|---|---|
| Source | its own session logs on disk | its language server, on loopback | `amp usage`, the vendor's own CLI | the usage endpoint, over the network |
| Credential needed | **none** | **none** | **none — Amp's key stays inside Amp** | yes — the OAuth token Claude Code stores |
| Network access needed | **none** | **none off this machine** | yes, but the request is Amp's, not ours | yes |
| Freshness | only as current as your last actual Codex call | live at the moment of reading | live at the moment of reading | live at the moment of reading |
| Reported as | `local log` | `local probe` | `via CLI` | `live query` |

Codex writes the rate-limit object the server hands it straight into
`~/.codex/sessions/**/rollout-*.jsonl`, so reading a file is enough.

Antigravity keeps no quota on disk, but it does not need to: the language
server it already runs will answer the question over `127.0.0.1`. That is the
cheapest live reading of the four — nothing is launched, no credential is
opened, and the request cannot leave the machine. The agent finds the server
the way any process finds another one it owns (on Linux by reading `/proc`, so
no `lsof` and no subprocess at all; on macOS with `ps` and `lsof`), reads the
`--csrf_token` the server was started with, and asks it
`RetrieveUserQuotaSummary`. The reply carries the two groups Antigravity's own
Model Quota UI shows — Gemini models and Claude/GPT models — each with a weekly
and a five-hour bucket, which is four rows under one heading.

The loopback port serves a self-signed certificate, so verification is off for
it. No certificate authority signs `127.0.0.1` for a local process, so there is
nothing verification could check; what bounds it is the address, which is a
literal loopback IP the connection cannot escape. The Google OAuth path — the
one that would read a credential and call `cloudcode-pa.googleapis.com` — is
deliberately **not** implemented, for the same reason the Amp bearer path is
not.

**Antigravity is not discovered on Windows**, and that is a refusal rather than
a gap left open. Finding the server is easy there — one call maps a listening
port to a pid — but the `--csrf_token` lives in another process's *command
line*, and reading one on Windows means either WMI (COM, or a PowerShell
subprocess costing a second or two inside a 30-second loop) or
`NtQueryInformationProcess` plus `ReadProcessMemory` to walk the target's PEB.
The first is too expensive for the loop it would run in. The second would put
"reads other processes' memory" into an agent whose whole claim is that it
reads files and makes one outbound request — and it would be there for every
user, whether or not they have ever opened Antigravity. Codex, Amp and Claude
Code all work on Windows: their quota is in a file or behind a CLI, and neither
needs anybody's process table.

Amp writes no balance to disk either, but it ships a CLI that will answer the
question itself, so this program runs `amp usage` and parses four lines of text.
That is the cheapest tier short of a local file: the API key in
`~/.local/share/amp/secrets.json` is never opened, never held, never sent — the
only thing crossing into this process is stdout. (Amp's first output line is the
signed-in email address. It is matched for shape and discarded; no capture group
in the parser can reach it, and a test asserts the address cannot appear in a
published payload.)

Claude Code writes **no quota to disk at all** — every nested key in recent
transcripts was walked to check, and only `service_tier` is there. So the
numbers have to be asked for, and asking requires the credential.

Of eight AI-tool config directories examined on one developer machine, **only
Codex persisted quota locally**. That is why tools in this space reach for OAuth
tokens rather than reading a file — but it is not the whole story, because
"ask the vendor's own CLI" is a third option that costs no credential at all,
and it is the one to prefer wherever a vendor offers it.

### Running another program

Amp support means this helper executes a binary, which everything else here
does not — the rest only reads files and makes one HTTPS request. The rules
that come with that:

- The binary is resolved to an **absolute path from a fixed list** (Amp's own
  two install locations plus the two usual package prefixes), not taken from
  `PATH`. `PATH` lookup remains only as a last resort, and `MON_AMP_BIN`
  overrides everything — an override that does not check out is a refusal, not
  a reason to run something else.
- A **world-writable** binary is refused. Group-writable is not, because that is
  the ordinary state of a Homebrew prefix.
- stdin is the null device (a CLI that decides to prompt hits EOF instead of
  hanging), there is a 15-second deadline, and output is read into a bounded
  buffer.
- Failures never quote the path being run: on a normal install that path
  carries the local username, and collector failures are published.

## The line that holds

**Reading a credential is not shipping one.**

- The Claude token is read from `~/.claude/.credentials.json`, used for exactly
  one request, and dropped. Never logged, never cached, never transmitted.
- Only four scalars are read out of `claudeAiOauth`. That file also holds MCP
  OAuth tokens for whatever third-party services the user has connected; this
  code stays away from them.
- The credential is **never refreshed and never written back**. An expired
  token is reported as expired and left for Claude Code to renew. Rewriting
  somebody's credential store to keep a status panel green is not a trade
  worth making.
- No error text derived from an exception, a URL, or a header is allowed into
  the output. Collector failures are *published* — they ride to the relay and
  render in a browser — and an HTTP layer quoting a whole `Authorization`
  header into an error string is exactly the shape that must never escape.
- No browser cookies are read. No Full Disk Access is requested.

What does travel: percentages, window lengths, reset times, and the plan/tier
name the quota belongs to. So the honest claim is *"no credential leaves the
machine"* — not *"only percentages leave the machine"*.

## Machine health

Alongside quota, the agent reports whether the machine itself is healthy. Every
field here had to clear one bar: **useful for deciding whether a machine needs
attention, useless for working out whose machine it is.**

Not collected, at all: hostname, username, paths, process lists, serial
numbers, MAC addresses, public IP. The kernel version is deliberately cut to
`major.minor` — "6.8" tells you whether a box is ancient; "6.8.0-1050-oracle"
also tells anyone who reads it which cloud to aim at and which CVEs to try.

Coverage is honest rather than uniform:

| | Linux | Windows | macOS | FreeBSD |
|---|---|---|---|---|
| CPU % | yes (differential, `/proc/stat`) | yes (`GetSystemTimes`) | — | — |
| Memory | yes (`MemAvailable`) | yes (`GlobalMemoryStatusEx`) | — | — |
| Swap | yes | **cannot** | — | — |
| Disk (root fs) | yes | yes (system volume) | yes | — |
| Load average | yes | **does not exist** | — | — |
| Uptime | yes | yes (`GetTickCount64`) | — | — |
| Cores / arch / kernel | yes | yes | yes | yes |

**macOS** reports far less, and the reason is structural rather than lazy.
Without cgo the Go standard library offers exactly two ways to read a sysctl:
`syscall.Sysctl`, which returns a string cut at the first NUL byte, and
`syscall.SysctlUint32`, which refuses anything that is not exactly four bytes
wide. Between them that rules out `hw.memsize` (64-bit, leading zero bytes),
`vm.loadavg` (a struct) and `kern.boottime` (a timeval). `CGO_ENABLED=0` is
what makes the Linux builds genuinely static, so the mach APIs that would
answer these are out of reach too.

There is a byte-layout trick that would usually recover the boot time. It is
deliberately **not** taken: it depends on a layout the author cannot test on
real hardware, and a silently wrong boot time reads as a machine that just
crashed.

**FreeBSD** is the same story with the same cause, plus one more: the read that
*would* work — `statfs` for the root filesystem — is written against the Linux
and Darwin field names, and the BSDs spell them `F_bsize` and `F_blocks`. A
version of it typed against a header nobody here can run is exactly the kind of
guess the rest of this list refuses.

**Windows** has two entries that are not "not yet" but "no":

- **Load average does not exist on Windows.** It is a Unix idea — a decaying
  average of the run queue — and the NT kernel has never kept one. "Processor
  Queue Length" is an instantaneous depth with different units, and putting it
  in a field labelled `load1` would put a number that means something else
  under a heading people read at a glance.
- **Swap has no honest reading.** `GlobalMemoryStatusEx` reports the *commit
  limit* and the commit charge, which is not the page file. The limit is
  roughly RAM plus the page file, so the usual move is to subtract — and it is
  a trick: non-paged pools and a page file Windows is free to grow both move
  it. A machine that reads 60% swapped when nothing has been paged out is worse
  than one that says it does not know.

The kernel version on Windows is `10.0` on both Windows 10 and Windows 11,
which is the true NT version and is deliberately as coarse as the Linux one.
The build number — the part that would tell you 23H2 from 24H2 — is also the
part that tells anyone reading it which patch level to try things against.

Anything a platform cannot measure is named in a `missing` list so the display
can say so, rather than rendering a zero that looks like an idle machine.

## Install

**Linux, macOS, FreeBSD**

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN>
```

**Windows** (PowerShell — not the older Command Prompt)

```powershell
& ([scriptblock]::Create((irm https://tools.subnsub.com/monitor/install.ps1))) -Token <TOKEN>
```

…and if you would rather look first, which is the reasonable instinct for
anything that installs a background process:

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh -o install.sh
less install.sh && sh install.sh <TOKEN>
```

```powershell
irm https://tools.subnsub.com/monitor/install.ps1 -OutFile install.ps1
notepad install.ps1 ; .\install.ps1 -Token <TOKEN>
```

It installs one binary to `~/.local/bin` and registers it to run at login — a
systemd **user** unit on Linux, a LaunchAgent on macOS, a **scheduled task** on
Windows. No sudo, no administrator rights, nothing written outside your home
directory. `sh install.sh --uninstall` (or `.\install.ps1 -Uninstall`) removes
both.

**Why a scheduled task and not a Windows service.** A service has to answer the
service control manager, which means either a second executable or a Go
dependency this project does not carry — and registering one needs
administrator rights nothing else about this install needs. The costs are named
rather than hidden: a task starts at logon rather than at boot unless it can be
registered to run whether or not you are signed in, which needs a privilege a
standard user usually lacks. The installer tries for that, falls back, and
prints which one your machine got.

A task also carries no environment, so on Windows the agent reads the token out
of the file the installer wrote. The two alternatives there were the task's
command line and the user's persistent environment, and both are readable by
every other program that user runs.

**FreeBSD registers nothing**, and says so. There is no per-user service
manager: the two ways to start something at boot are an `rc.d` script, which
needs root, and your crontab, which is outside this installer's promise to
write nothing outside your home directory. Quietly doing neither would be the
worst of the three, so the binary and the token are installed and the installer
prints the `daemon(8)` line and a ready-to-paste `rc.d` script.

Then open **[tools.subnsub.com/#monitor](https://tools.subnsub.com/#monitor)**
and sign in with the account the token came from. The machine appears within
about half a minute of the first push. Paste the same token on as many machines
as you like — each one gets its own card.

The installer carries the expected SHA-256 of every published binary **inline**.
That is the whole reason it is worth reading before piping into a shell: a
checksum fetched at install time from the same host as the binary
authenticates nothing — whoever can swap one can swap the other. These live in
the file you were invited to read.

> The copy of `install.sh` in this repository is a snapshot. The authoritative
> one is served from `tools.subnsub.com/monitor/install.sh` and is re-stamped
> with fresh checksums on every release, so the hashes here may lag a release
> behind.

## Build it yourself

```sh
cd go && sh build.sh          # → dist/subnsub-monitor-<goos>-<goarch>[.exe]
go test ./...
```

Builds are reproducible: `-trimpath` keeps absolute paths out and
`-buildvcs=false` keeps git state out. Build the same source anywhere and you
get the same bytes as the published release — check them against the
`SUM_*` lines in `install.sh`:

```sh
cd go && sh build.sh && sha256sum dist/*
```

That is what makes "verify it against the source" a real offer rather than a
slogan, and it is worth stating why the second flag is there: Go stamps the
commit hash and dirty flag into any binary built inside a work tree, so
without it the same source built from a checkout, a tarball and our own tree
produced three different binaries — and anyone taking the invitation to verify
would have found a mismatch with no way to tell an innocent stamp from a
swapped binary.

## Run it without installing anything

```sh
subnsub-monitor                   # one snapshot as JSON, exit
subnsub-monitor watch [SEC]       # reprint every SEC seconds
subnsub-monitor serve [PORT]      # serve /quota + /events on 127.0.0.1 only
subnsub-monitor selftest          # show what the collectors can and cannot open
subnsub-monitor token             # mint a relay token
subnsub-monitor name [LABEL]      # show or set this machine's dashboard name
subnsub-monitor console [on|off]  # may the dashboard run commands here?
subnsub-monitor update [on|off]   # may the dashboard replace this binary?
subnsub-monitor version           # print this build's version and exit
subnsub-monitor connect URL [TOKEN]
```

`serve` only ever shows you *this* machine, and binds to `127.0.0.1` — never
`0.0.0.0`. `connect` is the shape that matters.

Pass the token via `SUBNSUB_MONITOR_TOKEN` rather than as an argument to keep it
out of `ps`.

## One machine, one dashboard

You paste the same token on every machine you want to watch, so each snapshot
carries an `agent_id` that says which machine it came from. A relay that keeps
a slot per id shows them as separate dashboards; one that ignores the field
gets the old behaviour, where the last machine to push wins.

The id is created on first run and kept in `~/.config/subnsub-monitor/agent-id`.
**It is random, not derived** — no hostname, no MAC, no machine-id, nothing
hashed from any of them. That is deliberate and it is the same bar the health
fields clear: an id derived from hardware is a fingerprint that survives
reinstalls and can be correlated across accounts by whoever ends up holding the
relay's storage. Delete the file and you get a new one; that is the whole
extent of what it means. Set `MON_AGENT_ID` to pin it instead, which is what a
container image or a config-management run wants.

A name is optional and is the one piece of free text this agent sends. It is
never inferred — in particular never from the hostname, which would quietly
reintroduce exactly what the paragraph above rules out:

```sh
sh install.sh <TOKEN> --name "tokyo build box"   # at install time
subnsub-monitor name "tokyo build box"           # any time after
subnsub-monitor name                             # show what it will report
```

Control characters, bidi overrides and invisible padding are stripped before it
travels, and it is cut to 24 characters. A relay should re-do all of that on
arrival anyway — see the warning below.

## The console, and why it is off

The dashboard can run a command on a machine and show you the output. **This is
off unless you turn it on here, on the machine**, and that is the whole design
rather than a default someone might get around to changing:

```sh
sh install.sh <TOKEN> --console     # at install time
subnsub-monitor console on          # any time after
subnsub-monitor console off         # and back off
subnsub-monitor console             # show which it is
```

The switch is the existence of `~/.config/subnsub-monitor/console`, written by
someone who already had a shell here. With it absent, the agent never asks the
relay whether there is anything to run — so there is no channel to attack
rather than merely no permission to use one. `MON_CONSOLE=1` (or `0`) overrides
the file for one run, which is what a container wants.

The transport is the same outbound-only shape as the push: the agent asks, the
relay answers. Nothing listens on this machine, no port opens, and a machine
behind NAT works unchanged. What runs is `/bin/sh -c <line>` as the user this
agent runs as, in that user's home directory:

- each command is its own process — `cd` does not persist and neither does
  anything else. This is not a shell session, and it is not trying to be one;
- 30 seconds, then the whole process group is killed, so a command that
  backgrounded something does not leave it running;
- output is capped at 16 KB, combined stdout and stderr;
- the command's process group is taken down when it finishes, so a
  backgrounded process does not outlive the only thing that knew about it;
- **every command is written to this machine's log before it runs**, so the box
  keeps its own record of what was done to it that does not pass through
  anyone else. That log is the systemd journal on Linux and the launchd log on
  macOS; on Windows a scheduled task's output is discarded, so the agent writes
  its own file beside the config rather than letting the promise hold on two
  platforms out of three.

The shell is the platform's own — `/bin/sh -c` on Unix, `cmd.exe /s /c` on
Windows — and nothing translates between them. A Windows machine answers `dir`
and not `ls`. The alternative is an agent that quietly rewrites what somebody
typed, and there is no version of that which does not eventually run something
other than what was asked for. On Windows the tree is bounded with a **job
object** rather than a process group, which there is no such thing as.

Turning it off takes effect on a running agent within about ten seconds, and it
is re-checked between commands — so `console off` stops a batch already in
flight rather than being obeyed once that batch has finished.

On Linux the systemd unit's filesystem confinement depends on this setting, and
the installer prints which one it used. Without the console the service is
locked down to almost nothing (`ProtectSystem=strict`, `ProtectHome=read-only`);
with it on, the confinement is the ordinary one for a user service — a console
that could not write anything would not be security, it would be a console that
does not work. `NoNewPrivileges` holds either way.

An agent with the console off reports that fact in its snapshot, so a dashboard
can offer the feature only where it can be honoured.

## Updating itself, and who decides what that means

A new release should not mean logging into every machine. The dashboard can ask
an agent to replace its own binary — and the shape of that is one sentence:

> **The dashboard decides WHEN. The release bucket decides WHAT.**

Those are deliberately not the same authority. The message the dashboard sends
carries a machine id and a correlation id, and **nothing else** — there is no
field on it that could name a version, a URL, or a binary. So a relay that has
been taken over can make a machine update, and the only thing that machine will
do about it is fetch what was genuinely published, check it against the
published checksum, and run that. The base URL is a constant in
[`go/update.go`](go/update.go); no input moves it.

Which is also why **there is no timer**. An agent that went looking for releases
by itself would be a permanent, unattended download-and-execute channel on every
machine that ran it — including every machine whose operator never turned the
console on. That is strictly more authority than the console grants, acquired by
accident. Nothing is downloaded until somebody presses the button.

```sh
sh install.sh <TOKEN> --remote-update   # at install time
subnsub-monitor update on               # any time after
subnsub-monitor update off              # and back off
subnsub-monitor update                  # show which it is
```

`--console` implies it. A machine with the console on has already handed the
dashboard an arbitrary `/bin/sh`, and `curl …/install.sh | sh` typed at that
prompt is this same operation with fewer checks and no audit line — refusing the
narrow version of something already permitted in its broadest form would be
theatre. The separate switch is for the other machine: no console, but willing
to be upgraded. That one is a genuinely new grant, so it is its own file and it
is off until you set it.

What happens when it runs, in order, and it stops at the first thing that fails:

1. read the published version; refuse anything that is not `YYYY.MM.DD.N`;
2. refuse to go **backwards** — a bucket serving an older build is either a
   rollback somebody should perform deliberately or an attempt to walk a fleet
   onto a build with a known hole;
3. download, and check the SHA-256 against the published manifest;
4. **run the new binary** and require it to agree about its own version — a
   build for the wrong architecture passes every check above and fails here,
   which is much better than failing after the swap as a service that no longer
   starts on a machine you cannot reach;
5. hard-link the running binary aside, then replace it with **one atomic
   rename**. The backup is a link and not a move on purpose: renaming the old
   one out of the way first would leave the service path empty for an instant,
   and a power cut inside that instant leaves a machine whose `ExecStart` names
   a file that is not there.

   **Windows cannot do this**, and inverts the order instead. The loader keeps
   the running image open, so the file may be renamed but not replaced in
   place — the old binary is moved aside first and the new one moved in, which
   leaves exactly the instant described above. It is one rename wide and there
   is no way to close it on that platform. What *is* closed is the ordinary
   failure: if the second rename fails for any reason short of the machine
   stopping, the first is undone before the agent reports anything;
6. **report the result, then exit.** This is the one command whose success kills
   the thing that would have reported it, so the report goes first — losing the
   message in exactly the case where the swap worked would be indistinguishable
   from an agent that died. `systemd`/`launchd` starts the new one.

The previous binary stays as `<binary>.prev`. On Linux the systemd confinement
depends on this switch too, because the strict sandbox correctly makes the
agent's own binary unwritable by it: `--remote-update` opens `ReadWritePaths`
for the install directory and nothing else, and `subnsub-monitor update` tells
you when the switch and the unit disagree.

## Pointing it at your own relay

The relay this repository's installer defaults to is ours, and it will only
accept tokens issued to accounts entitled to use it. **You do not need it.**
The agent talks to any endpoint that implements two things:

- `POST /push` with `Authorization: Bearer <token>` and a JSON body — the
  snapshot as printed by `subnsub-monitor` with no arguments. Answer `200` to
  accept. Non-2xx makes the agent back off exponentially.
- Whatever you like for delivery to a viewer. The agent does not care.

Nothing in the push response is read. The status code is the entire protocol in
that direction, and that is deliberate: a helper that parsed what a relay sent
back would be a helper a compromised relay could steer, on a machine where it
can read the credential files it reports on. If you are writing a relay, you
cannot tell this agent anything — by design.

### Token renewal (optional, and not the relay's business)

Tokens issued by our site expire. Rather than let every machine on an account go
dark on the same day, the agent trades its token in for a fresh one shortly
before that, against the site that issued it — never against the relay, which
has no idea who anyone is:

**If you point the agent at your own relay, renewal is OFF.** Your relay's token
is not ours and is not ours to present anywhere; the agent will not POST it to
our site because your relay answered `403`. Renewal happens only when the relay
is the one this installer ships with, or when you name your own issuer with
`SUBNSUB_MONITOR_SITE=https://…` — which the installer validates and writes into
the service definition, so it survives into the background process rather than
being lost with the shell you typed it in. A token with no expiry field is never
renewed either, so a relay that mints its own tokens needs none of this.

```sh
MON_RELAY=https://relay.example.com \
SUBNSUB_MONITOR_SITE=https://issuer.example.com \
  sh install.sh <TOKEN>
```

When it is on:

- `POST /api/monitor-token/renew` with `Authorization: Bearer <current token>`,
  answered with `{"token": "...", "expires_at": <unix seconds>}`.
- The replacement is accepted only if it matches the token alphabet
  (`[A-Za-z0-9_-]{24,128}`), names **the same account** as the token held,
  expires strictly later than it, and does not claim an implausible lifetime.
  Nothing else in that response is looked at. The account check is what stops a
  wrong answer from quietly moving a machine's readings into someone else's
  dashboard; the lifetime cap is because the stored token beats the installed
  one *by expiry*, so a forged far-future date would win every restart forever.
- It is used immediately but **written to disk only after a push made with it has
  been accepted**, and reverted if the relay refuses it. The issuer and the relay
  verify with one secret, and rotating that secret is exactly the window where
  the site issues tokens the relay rejects — committing on receipt would
  overwrite the only working credential with a broken one.
- The file is `~/.config/subnsub-monitor/token.current`. It records the relay it
  belongs to, and is ignored for any other one.

```sh
MON_RELAY=https://relay.example.com sh install.sh <TOKEN>
# or, without installing:
subnsub-monitor connect https://relay.example.com <TOKEN>
```

A token is a bearer secret: whoever holds it can push readings and read them.
`subnsub-monitor token` mints one from the system CSPRNG.

Treat everything a helper pushes as hostile input. The protocol is public and
this agent is open source, so anyone can POST anything at your relay —
rebuild the payload field by field from a whitelist rather than storing what
arrived, or you have built an XSS delivery service with a CDN in front of it.

## Reference implementation

`reference/monitor.py` is the original Python proof of concept. It is kept
because it is where the file formats, the endpoint, and every trap five review
rounds turned up were actually found. The Go port is what ships; the Python is
what explains it.

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution.

Apache rather than a copyleft licence on purpose: this is a thing people run
on work machines, and AGPL gets flagged by corporate compliance scanners in a
way that would exclude an entire class of the people it is for.
