# mole

A tiny CLI that deploys a VPS and spins up a CN-direct VPN through it — deploy, connect, done.

Built on [sing-box](https://sing-box.sagernet.org/) with CN-direct routing baked in.

## Install

```bash
# sing-box runtime (required)
brew install sing-box

# mole itself — pick one:
go install github.com/LeonTing1010/mole@latest                # Go toolchain
# — or —
git clone https://github.com/LeonTing1010/mole && cd mole && sudo make install
```

## Usage

```bash
mole server deploy              # deploy a VPS (Vultr / DigitalOcean / …)
mole server ls                 # list deployed VPS
mole server use <name>         # pick the active server
mole up                        # connect (runs in background)
mole down                      # disconnect
mole status                    # show active server, mode and exit IP
mole ceiling [mbps|auto]       # pin / show the Brutal down-ceiling (no reconnect)
```

`mole server` subcommands: `deploy`, `destroy <name>`, `ls`, `reconcile`,
`adopt <instance-id>`, `use <name>`.

Config lives in `~/.mole/`: `servers.json` (deployed VPS list + per-server
tuning), `config.yaml` (credentials), `mole.log` (sing-box output), and cached
rule-sets / curated direct-domain lists.

## Bandwidth control

mole runs Hysteria2 with the **Brutal** congestion controller. Setting
`up_mbps` / `down_mbps` in a server's `servers.json` entry caps the declared
bandwidth so Brutal stops flooding a congested link — set them to ~80% of a real
speed test, or to `-1` to disable Brutal and let Hysteria2 fall back to adaptive
BBR (best for time-varying links).

Because sing-box bakes an outbound's bandwidth in at config load, the ceilings
are **materialized as a selector ladder** (`proxy-bw-2`, `proxy-bw-5`, …) at
`mole up` time. You can then switch between rungs — or hand control back to the
clock — without reconnecting:

```bash
mole ceiling            # show current ceiling + available rungs
mole ceiling 20         # pin the down-ceiling to 20 Mbps
mole ceiling auto       # back to the time-of-day schedule
```

The switch is a single Clash-API call, so the TUN, DNS and in-flight
connections are untouched. Changing the ladder itself (the `servers.json`
`up_mbps`/`down_mbps`/peak values) still needs `mole down && mole up`.

### Time-of-day peak window

If a server sets `peak_down_mbps > 0`, `mole up` emits a peak/off-peak selector
and the supervisor flips it on the local clock with no reconnect —
`up_mbps`/`down_mbps` become the off-peak ceiling and the `peak_*` values the
peak one (set the peak ceiling to ~worst-case link speed so Brutal stops
flooding at busy hours). Hours are local 24h; leaving `peak_start_hour` and
`peak_end_hour` at `0` uses the default `12:00–02:00` window.

## Routing & DNS

Routing is CN-direct by design: China IP ranges (`geoip-cn`) go out the direct
interface, everything else through the VPS. DNS uses a fake-IP generator for
foreign names so resolution never depends on a reachable upstream, while known
Chinese domains — including domestic sites on `.com` TLDs — resolve to their
**real** IP via AliDNS (`geosite-cn`) so they route direct instead of abroad.

Rule-sets (`geoip-cn`, `geosite-cn`) and the curated direct-domain list are
**prefetched locally** at `mole up`. A failed prefetch degrades gracefully
(mole still starts; that classification is just skipped) rather than refusing to
start, so a dead VPS never becomes a hard startup failure.

## Requirements

- macOS or Linux (TUN requires root; `mole up` invokes `sudo sing-box`)
- Go 1.21+ to build from source
- sing-box 1.11+ in `$PATH`

## License

MIT
