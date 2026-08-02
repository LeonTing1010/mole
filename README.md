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
mole server deploy              # deploy a VPS (provisioned via mole-server)
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

A measured ceiling outlives the server that carried it. Destroy records the
ladder in `~/.mole/ladders.json` keyed by **region**, and the next deploy into
that region starts from it instead of the conservative defaults — otherwise
destroy-on-lid-close would throw away an afternoon of measuring every night and
quietly run the next day's tunnel at a fraction of what the path can carry.
Nothing is inherited across regions, because the number describes a transit
path and not a box. An inherited ceiling the new instance cannot hold is not a
trap: the supervisor backs it down a rung at a time within minutes. One that is
too low has nothing to correct it, which is the asymmetry the record exists
for.

## Destroy on lid close

Vultr bills an instance for existing, not for running — a *stopped* instance
costs exactly as much as a running one, and the only way to stop paying is to
destroy it. So mole destroys the server when you shut the lid. **This is on by
default.**

```bash
mole up                            # server is deleted when the lid closes
mole up --no-destroy-on-lid-close  # keep it (and keep paying)
mole down --destroy                # stop the VPN and delete the server now
```

The lid is the trigger because it is the only signal that is not a guess.
Traffic is an unreliable stand-in for "this tunnel is wanted" in both
directions — a machine at the login screen still chatters to push channels, and
a peak-hour UDP blackout looks exactly like a quiet evening. A
time-since-last-keypress timer has to pick a threshold that is wrong for
somebody, and worse, it cannot survive the thing it is waiting for: once the
host sleeps the daemon is frozen, the count stops where it stood, and the
server bills until morning.

A closing lid is none of that. Someone decided with their hands, the system
says so, and it *waits* for an answer — the network is still up when the
notification arrives, so the delete completes before the machine goes down.
mole registers for the power-management notification through IOKit
(`IORegisterForSystemPower`) and never vetoes the sleep itself: a laptop that
refuses to sleep in a bag is a far worse outcome than any billing this saves.
macOS only; elsewhere the feature reports that it is unavailable rather than
arming a trigger that will never fire.

Sleep alone is *not* the trigger, though it used to be, and that was a bug: a
laptop sleeps dozens of times a day without anyone leaving — dark-wake
maintenance, sleep services, and the idle sleep that follows a blanked screen
all arrive as the same `kIOMessageSystemWillSleep`. So each one is checked
against `AppleClamshellState` (and root domain's `Sleep Reason`) at the moment
it arrives, and only a genuinely shut lid destroys anything. A lid closed with
an external display attached, where the machine stays awake, does not — the
tunnel is still in use.

Polling that same lid switch on a timer would not work, which is why it is read
here and not on a loop: the machine sleeps a second or two after the lid shuts,
and no polling interval fast enough to notice leaves time to finish a
cross-border request.

Coming back is manual and takes a couple of minutes:

```bash
mole server deploy && mole up
```

Worth knowing:

- Vultr's hourly rate is the monthly price ÷ 672 and is **capped at the monthly
  price**, so you have to be destroyed for more than ~48 hours a month before
  saving a cent. Every instance also carries a **1-hour minimum charge**.
- Each redeploy is a **new IP and a new identity**. Keeping an IP across
  destroys means a reserved IP, which Vultr bills at ~$3/mo — more than this
  feature saves.
- Nothing covers the host being shut down outright rather than slept. That
  would need a watcher that is always on somewhere else, and putting a Vultr
  key back into an internet-facing service costs more than this saves.
- It needs `VULTR_API_KEY` in the environment at `mole up` time. TUN needs
  root, `mole up` re-execs through sudo, and sudo drops the environment, so the
  key is stashed at `~/.mole/vultr_key` (0600) for the daemon and removed when
  it exits. That is a second copy of a root-scoped credential on disk — a good
  reason to give mole a Vultr key restricted to instance management.

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

## Kill switch (macOS)

sing-box's TUN routes only protect while sing-box is alive — if it dies, its
routes vanish and foreign connections silently fall back to the physical
interface. `mole up` therefore arms a PF anchor (`com.apple/mole`) **before**
starting sing-box: physical-interface egress is blocked except for the VPS
endpoint, China IPv4 space (kept in sync weekly, embedded fallback), the LAN,
and DHCP. Domestic traffic stays direct and unrestricted at all times; foreign
traffic can only exist inside the tunnel — a dead sing-box means foreign is
*blocked*, not leaked (fail-closed).

`mole down` disarms it. If the daemon is SIGKILL'd the anchor deliberately
stays armed (that's the point); `mole status` calls this out, and the next
`mole up`/`mole down` resolves it. Opt out with `mole up --no-killswitch`.

## Requirements

- macOS or Linux (TUN requires root; `mole up` invokes `sudo sing-box`)
- Go 1.21+ to build from source
- sing-box 1.11+ in `$PATH`

## License

MIT
