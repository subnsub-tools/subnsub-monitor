# codex-meter

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
  │  codex-meter     │──POST /push──▶ relay ◀──WS──│  browser  │
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

Two providers, and the difference between them is the single most important
fact about this program:

| | Codex | Claude Code |
|---|---|---|
| Source | its own session logs on disk | the usage endpoint, over the network |
| Credential needed | **none** | yes — the OAuth token Claude Code stores |
| Network access needed | **none** | yes |
| Freshness | only as current as your last actual Codex call | live at the moment of reading |
| Reported as | `local log` | `live query` |

Codex writes the rate-limit object the server hands it straight into
`~/.codex/sessions/**/rollout-*.jsonl`, so reading a file is enough.

Claude Code writes **no quota to disk at all** — every nested key in recent
transcripts was walked to check, and only `service_tier` is there. So the
numbers have to be asked for, and asking requires the credential.

Of eight AI-tool config directories examined on one developer machine, **only
Codex persisted quota locally**. If you are wondering why tools in this space
reach for OAuth tokens rather than reading a file: for every provider but one,
there is no file.

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
curl -fsSL https://tools.subnsub.com/meter/install.sh | sh -s -- <TOKEN>
```

…and if you would rather look first, which is the reasonable instinct for
anything that installs a background process:

```sh
curl -fsSL https://tools.subnsub.com/meter/install.sh -o install.sh
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
> one is served from `tools.subnsub.com/meter/install.sh` and is re-stamped
> with fresh checksums on every release, so the hashes here may lag a release
> behind.

## Build it yourself

```sh
cd go && sh build.sh          # → dist/codex-meter-{linux,darwin}-{amd64,arm64}
go test ./...
```

Builds use `-trimpath`, so two people building the same commit get the same
bytes. That is what makes "verify it against the source" a real offer rather
than a slogan.

## Run it without installing anything

```sh
codex-meter                   # one snapshot as JSON, exit
codex-meter watch [SEC]       # reprint every SEC seconds
codex-meter serve [PORT]      # serve /quota + /events on 127.0.0.1 only
codex-meter selftest          # show what the collectors can and cannot open
codex-meter token             # mint a relay token
codex-meter connect URL [TOKEN]
```

`serve` only ever shows you *this* machine, and binds to `127.0.0.1` — never
`0.0.0.0`. `connect` is the shape that matters.

Pass the token via `CODEX_METER_TOKEN` rather than as an argument to keep it
out of `ps`.

## Pointing it at your own relay

The relay this repository's installer defaults to is ours, and it will only
accept tokens issued to accounts entitled to use it. **You do not need it.**
The agent talks to any endpoint that implements two things:

- `POST /push` with `Authorization: Bearer <token>` and a JSON body — the
  snapshot as printed by `codex-meter` with no arguments. Answer `200` to
  accept. Non-2xx makes the agent back off exponentially.
- Whatever you like for delivery to a viewer. The agent does not care.

```sh
CM_RELAY=https://relay.example.com sh install.sh <TOKEN>
# or, without installing:
codex-meter connect https://relay.example.com <TOKEN>
```

A token is a bearer secret: whoever holds it can push readings and read them.
`codex-meter token` mints one from the system CSPRNG.

Treat everything a helper pushes as hostile input. The protocol is public and
this agent is open source, so anyone can POST anything at your relay —
rebuild the payload field by field from a whitelist rather than storing what
arrived, or you have built an XSS delivery service with a CDN in front of it.

## Reference implementation

`reference/codex-meter.py` is the original Python proof of concept. It is kept
because it is where the file formats, the endpoint, and every trap five review
rounds turned up were actually found. The Go port is what ships; the Python is
what explains it.

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution.

Apache rather than a copyleft licence on purpose: this is a thing people run
on work machines, and AGPL gets flagged by corporate compliance scanners in a
way that would exclude an entire class of the people it is for.
