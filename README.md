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

Three providers, and what each one *costs to read* is the single most important
fact about this program:

| | Codex | Amp | Claude Code |
|---|---|---|---|
| Source | its own session logs on disk | `amp usage`, the vendor's own CLI | the usage endpoint, over the network |
| Credential needed | **none** | **none — Amp's key stays inside Amp** | yes — the OAuth token Claude Code stores |
| Network access needed | **none** | yes, but the request is Amp's, not ours | yes |
| Freshness | only as current as your last actual Codex call | live at the moment of reading | live at the moment of reading |
| Reported as | `local log` | `via CLI` | `live query` |

Codex writes the rate-limit object the server hands it straight into
`~/.codex/sessions/**/rollout-*.jsonl`, so reading a file is enough.

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

| | Linux | macOS |
|---|---|---|
| CPU % | yes (differential, `/proc/stat`) | — |
| Memory | yes (`MemAvailable`) | — |
| Swap | yes | — |
| Disk (root fs) | yes | yes |
| Load average | yes | — |
| Uptime | yes | — |
| Cores / arch / kernel | yes | yes |

macOS reports far less, and the reason is structural rather than lazy. Without
cgo the Go standard library offers exactly two ways to read a sysctl:
`syscall.Sysctl`, which returns a string cut at the first NUL byte, and
`syscall.SysctlUint32`, which refuses anything that is not exactly four bytes
wide. Between them that rules out `hw.memsize` (64-bit, leading zero bytes),
`vm.loadavg` (a struct) and `kern.boottime` (a timeval). `CGO_ENABLED=0` is
what makes the Linux builds genuinely static, so the mach APIs that would
answer these are out of reach too.

There is a byte-layout trick that would usually recover the boot time. It is
deliberately **not** taken: it depends on a layout the author cannot test on
real hardware, and a silently wrong boot time reads as a machine that just
crashed. Anything a platform cannot measure is named in a `missing` list so the
display can say so, rather than rendering a zero that looks like an idle
machine.

## Install

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN>
```

…and if you would rather look first, which is the reasonable instinct for
anything that installs a background process:

```sh
curl -fsSL https://tools.subnsub.com/monitor/install.sh -o install.sh
less install.sh && sh install.sh <TOKEN>
```

It installs one static binary to `~/.local/bin` and registers it to run at
login — a systemd **user** unit on Linux, a LaunchAgent on macOS. No sudo,
nothing written outside your home directory. `sh install.sh --uninstall`
removes both.

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
cd go && sh build.sh          # → dist/subnsub-monitor-{linux,darwin}-{amd64,arm64}
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
- **every command is written to this machine's log before it runs**, so the box
  keeps its own record of what was done to it that does not pass through
  anyone else.

On Linux the systemd unit's filesystem confinement depends on this setting, and
the installer prints which one it used. Without the console the service is
locked down to almost nothing (`ProtectSystem=strict`, `ProtectHome=read-only`);
with it on, the confinement is the ordinary one for a user service — a console
that could not write anything would not be security, it would be a console that
does not work. `NoNewPrivileges` holds either way.

An agent with the console off reports that fact in its snapshot, so a dashboard
can offer the feature only where it can be honoured.

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
