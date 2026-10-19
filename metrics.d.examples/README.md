# Ready-made tiles

Each file here is one source for `~/.config/subnsub-monitor/metrics.d/`. Copy
the ones that match the machine, check them, and the tiles are on its card
with the next report — no restart:

```sh
mkdir -p ~/.config/subnsub-monitor/metrics.d
cp nvidia-gpu.json ~/.config/subnsub-monitor/metrics.d/
subnsub-monitor metrics          # runs every command once and prints the tiles
```

The field reference is in the helper README under "Your own tiles". Every
command here runs as the helper's own user through `/bin/sh`, from its home
directory, with the helper's `PATH`.

| File | Tiles | Needs |
|---|---|---|
| `nvidia-gpu.json` | per GPU: temperature, load, memory, power | `nvidia-smi` |
| `sensors.json` | CPU temperature, every spinning fan | `sensors` (lm-sensors) |
| `ups-nut.json` | line/battery state, charge, runtime, load | `upsc` (NUT); edit `UPS=ups@localhost` |
| `docker.json` | running containers, unhealthy / restarting / dead, image count | `docker`, and the helper's user in the `docker` group |
| `systemd-failed.json` | failed system units, first two named | `systemctl` |
| `smart.json` | one health tile per disk | `smartctl`, and passwordless sudo for it (below) |
| `zfs.json` | per pool: health and capacity | `zpool` |
| `pihole.json` | blocked share, queries, blocked, blocklist size | Pi-hole v6 (`pihole api`), `jq` |
| `latency.json` | ping and packet loss to 1.1.1.1, HTTPS time to example.com | `ping`, `curl`; edit the targets |
| `quota-demo.json` | a quota of your own, as an agent on the board (helper README, "A quota of your own") — the command prints fixed numbers; replace it with one that reads the platform | `sh` |
| `cloudflare.json` + `cloudflare-bill.sh` | Cloudflare Workers Paid as an agent: this period's projected bill against your budget, then the free allowances past their limit or closest to it | `curl`, `jq`, a read-only API token (below) |

Limits to keep in mind when combining them: 8 files, 32 tiles and 8 tiles
with `"history": true` per machine, counted in file-name order. What is over
the line is left out — the ninth file, the thirty-third tile — and a ninth
history tile still shows its value, without a trend. Drop `"history": true`
from the tiles whose trend you do not need.

## Notes

**smart.json** reads disk health, which needs root. Allow exactly that and
nothing else, then check with `sudo -n smartctl --scan`:

```
# /etc/sudoers.d/subnsub-monitor-smart   (replace `me` with the helper's user)
me ALL=(root) NOPASSWD: /usr/sbin/smartctl --scan, /usr/sbin/smartctl -H /dev/*
```

**pihole.json** is for Pi-hole v6. On v5 replace the command's first half with
`curl -s "http://localhost/admin/api.php?summaryRaw&auth=YOUR_API_TOKEN"` and
the jq paths with `.ads_percentage_today`, `.dns_queries_today`,
`.ads_blocked_today` and `.domains_being_blocked`.

**cloudflare.json** runs `cloudflare-bill.sh` from the same directory, so
copy both (the helper reads only the `.json` files there). The script wants
an API token with two read-only permissions — Account Analytics: Read and
Billing: Read — for one account, kept in a file only the helper's user can
read:

```sh
install -m 600 /dev/null ~/.config/subnsub-monitor/cloudflare.env
cat >> ~/.config/subnsub-monitor/cloudflare.env <<'END'
CF_API_TOKEN=your-token
CF_ACCOUNT_ID=your-account-id
CF_BUDGET=10
END
sh ~/.config/subnsub-monitor/metrics.d/cloudflare-bill.sh   # prints the reading
```

`CF_BUDGET` (US dollars a billing period) is the bill row's total;
`CF_ROWS` (2…8, default 4) how many rows the card shows; `CF_LANG=zh` names
the rows in Chinese. What the estimate counts, and what it leaves out, is at
the top of the script. The card's money is a projection — usage so far, run
on to the end of the period — and the invoice is the authority.

**A tool that is not installed** leaves its tiles dimmed with `command
failed` — delete the file, or the tile, rather than living with it.

**Several tiles from one command**: most files here print a JSON array, so
one run of `nvidia-smi` feeds every GPU tile. Ids come from the device
(`gpu0_temp`, `zfs_tank_used`), and a tile's history hangs off its id — rename
a pool and its trend starts over.

Any of these tiles can be pinned to the folded status bar from the card's
Edit, like the built-in readings.

How these were checked: `docker.json`, `systemd-failed.json` and
`latency.json` against a live Linux machine; the rest against recorded output
of each tool, through the helper's own `subnsub-monitor metrics`. If your
tool's output differs, that command prints exactly what the helper could not
read.
