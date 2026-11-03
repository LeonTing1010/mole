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
mole server deploy              # create a VPS and verify its requested tunnels
mole server ls                 # list deployed VPS
mole server use <name>         # pick the active server
mole server link [name]        # print the vless:// link another client can import
mole up                        # connect (runs in background)
mole down                      # disconnect
mole status                    # show active server, mode and exit IP
mole ceiling [mbps|auto]       # pin / show the Brutal down-ceiling (no reconnect)
```

`mole server` subcommands: `deploy`, `destroy <name>`, `ls`, `reconcile`,
`adopt <instance-id>`, `use <name>`, `link [name]`.

Deployment waits up to six minutes after instance creation for its address and
requested tunnels; `--timeout 8m` changes that budget. Each protocol must
successfully authenticate and carry an HTTPS response. Progress appears every
15 seconds. A timeout or partially working node exits nonzero and retains the
instance for diagnosis; it may still be billing. Resume the check with
`mole server verify <name> --timeout 6m`, or delete it with
`mole server destroy <name>`. Verification never creates or repairs a VPS.
`--no-verify` explicitly skips this acceptance step and records `unverified`.
`server ls` distinguishes `provisioning`, `unverified`, `degraded`, and `ready`;
`ready` describes the last verification, not a continuous health guarantee.

Ctrl-C during a deploy cancels verification, waits for its helper to exit, then
deletes the created instance. A failed deletion remains recorded as
`cleanup_pending`. Ctrl-C during `server verify` leaves the instance intact.
Deployment phase logs and receipts live under `~/.mole/deployments/<deploy-id>/`.
The private `identity.json` is recovery material and must not be shared; the
receipt and phase log exclude credentials.

HY2 2.13.0 and sing-box 1.14.4 are installed from fixed, SHA-256-verified release
artifacts. Required installation has a four-minute node-side budget. TCP Brutal
2.0.1 installs separately after REALITY starts, with its own five-minute budget;
optimization failure does not stop either tunnel. Node logs are
`/var/log/mole/boot.log`, `/var/log/mole/brutal-install.log`, and
`/var/log/mole/brutal.status`. Version upgrades must update the artifact digests
in `cmd/nodeinstall.go` and the pinned Brutal source/archive in `cmd/brutal.go`.

Dual-protocol boot prepares SSH access first, then runs the HY2 and REALITY
installers concurrently under the same deadline. Downloads overlap; service
account and package mutations share a bounded lock. HY2 starts before optional
service cleanup. Timestamped `installing` and `service_active` events in the
node boot log describe local installation; only external authenticated HTTPS
checks establish deployment readiness. Cloud boot time remains provider-dependent.

Config lives in `~/.mole/`: `servers.json` (deployed VPS list + per-server
tuning), `config.yaml` (credentials), `mole.log` (sing-box output), and cached
rule-sets / curated direct-domain lists.

## Recovery behavior

Warm and fresh-connection monitors race two independently validated HTTPS
origins under one deadline. One origin outage cannot trigger a transport switch.
Recovery from HY2 commits the REALITY selector only after three spaced
successful authenticated checks; a failed candidate or selector write retains
fallback evidence. Optional TCP pacing confirmation is reported separately and
does not block a transport that delivers authenticated traffic.

FakeIP mappings persist in `~/.mole/sing-box-cache.db` so cached application DNS
results survive an engine restart. This file contains DNS metadata and remains
in a mode-0600 file. Before restarting a qualified engine, Mole checks the offline
cache's committed mappings and repairs missing or stale allocator metadata,
with up to three private recovery backups. A live lock, unknown format, or
inconsistent mappings are refused without clearing the cache. State snapshots
use unique atomic temporary files.
HY2 packet defaults remain conservative until same-node measurements justify a
change; the server configuration does not emit an unsupported root `mtu` key.

Mole qualifies stable sing-box 1.14.x starting at 1.14.3 and checks generated
configuration before taking over DNS or changing network settings. The maintained
local engine candidate is 1.14.4; a new minor or prerelease requires qualification
before use. Native IPv6 peers receive exact `/128` TCP pacing rules, with mapped
IPv4 normalized to `/32`.

For the controlled 400/500/600 Mbps comparison and its limits, see
[the stack and ceiling measurements](docs/performance/2026-11-03-stack-ceiling.md).

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

For a node with both legs, `reality_down_mbps` in its `servers.json` entry sets
the node-side TCP Brutal rate independently of the Hysteria2 ladder. If omitted,
REALITY follows the selected Hysteria2 ceiling as before. Changing this value
takes effect after `mole down && mole up`.

`reality_multiplex: true` opts the TCP leg into sing-box's native **smux**
session pool (up to eight connections, one active stream before expanding).
The node's VLESS inbound must first have `multiplex: {"enabled": true}`;
newly generated node configs include this support, while older nodes need an
explicit server upgrade. Unset keeps the existing non-multiplexed client.
The option is carried in Mole links as `mux=smux`, preserves Vision and REALITY
authentication, and takes effect after reconnecting. Idle TCP/REALITY sessions
can serve new application flows without another outer handshake. This helps
setup latency and some low-concurrency transfers; TCP loss still affects streams
sharing a session, and throughput gains depend on the path. Measurements and
limits: [2026-10-20 performance investigation](docs/performance/2026-10-20.md).

The switch is a single Clash-API call, so the TUN, DNS and in-flight
connections are untouched. Changing the ladder itself (the `servers.json`
`up_mbps`/`down_mbps`/peak values) still needs `mole down && mole up`.

On a node with both transports, failover compares three spaced fresh keeper
requests **and real external HTTPS delivery** on each leg. Cloudflare trace
and Google's connectivity endpoint race over separate fixed loopback forwarders;
one successful, validated response is enough, and the losing probe is canceled.
TLS certificate verification stays enabled. The keeper proves body delivery,
including when the external connectivity endpoint legitimately returns HTTP 204.
The UDP probe
uses the actual scheduled or active ladder rung, rather than always testing
the maximum ceiling. Fresh-flow failures on the fallback can trigger an early
return to a verified healthy primary; the normal dwell still applies while
the fallback is healthy. A failed control-API write retains the old routing
state. Monitor connections and in-flight probe evidence are cleared at a
transport handoff so they cannot report health from the leg just left.

`mole leg both` assigns separate connections through eight source-port buckets.
When both legs qualify, the fixed allocation is six REALITY and two HY2 buckets.
Three consecutive probe failures or slow replies withdraw a leg; recovery requires
three consecutive clean checks. A single error does not withdraw a qualified leg.
The controller does not sample bulk throughput or dynamically weight capacity.
Bucket percentages are not measured byte or connection shares. Existing connections
retain their transport. `mole leg auto`, `reality` and `hy2` restore single-route
controls; `auto` keeps REALITY primary with HY2 fallback. Upgrades need one
`mole down` / `mole up` to run the new daemon; subsequent mode changes do not.

Local UDP `ENOBUFS` pressure is checked on the dual controller's next 15-second
tick without consuming the minute-scale self-healing counter. It lowers the
active HY2 rung at most once per minute and pauses new UDP allocations for
30 seconds when TCP is qualified. Repeated pressure can descend to the lowest
configured rung; 15 minutes of clean UDP health restores one rung. The lower
scheduled/operator ceiling still applies. Stall rotation starts from the actual
HY2 rung and cannot raise a pressure-limited lowest rung. Status shows the
active down/up pair, pressure cap, and cooldown. Existing flows retain their leg;
this mitigates local pressure rather than guaranteeing packet delivery.

The protocols share the node and local uplink, so their ceilings are not additive
bandwidth. A single connection uses one protocol; multiple connections can use both.
Physical bottlenecks and path outages still apply.

Probe status separates successful replies above 1000 ms from failed requests.
These synthetic requests do not describe every application or establish an outage.
Custom domain routing reloads retry failures with backoff up to 60 seconds; malformed
custom JSON does not overwrite the last good sets. Rule-set files are replaced
individually, not as a multi-file transaction. DNS domain policy is baked into the
startup config and still requires `mole down && mole up` after a domain policy change.

For a bounded measurement, run `python3 scripts/measure-dual.py` in a normal
terminal with Mole running. It holds total concurrency constant: eight downloads
on Reality, eight on HY2, then four on each simultaneously through the owned-node
keeper forwarders. `--connections 2|4|8|16` changes the total for every phase.
Each phase lasts up to 15 seconds plus pending socket timeouts and reserves at
most 256 MiB (768 MiB total). Reports separate reserved and delivered bytes, started/completed/failed/cutoff requests,
and stop reasons. Deadline-cutoff transfers are incomplete even with zero transport
errors and return exit status 2. New `ENOBUFS` stops the test; failures are preserved
in `/tmp/mole-dual-measure.json` and produce exit status 2. Combined throughput
uses one shared elapsed interval; it does not sum sequential measurements. This
measures VPS-to-client payload, not the provider's port limit or local upload.
The direct forwarders deliberately test both legs regardless of routing share;
live scheduler changes are recorded alongside each phase. For longer tests use
`--seconds 30 --max-mib 512`, after the short run stays clean.

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

## Using a node from another client

`mole server link` prints the node's **VLESS+REALITY** leg as a standard
`vless://` link, which Hiddify, v2rayN and NekoBox import as-is. REALITY
authenticates with the X25519 key already in the link rather than a certificate
chain, so nothing is downgraded by handing it out — but the uuid in it *is* the
credential, so treat the line as a secret.

The hysteria2 link is deliberately not the default. It is mole's own
interchange format: the Brutal ceiling, the peak window, the port-hop range and
the pinned certificate all ride in query parameters only mole's parser reads.
Another client drops every one of them silently, and the pinned node is the case
that breaks rather than merely degrades — because mole pins, it omits
`insecure=1`, so the importer ends up verifying a self-signed certificate
against the public roots, and the handshake fails with nothing pointing at the
parameter it never saw.

`mole server link --hy2` prints that leg anyway, in the plain dialect every
client speaks, and it can only do so with `insecure=1`: a pin has no standard
spelling. Hysteria2 sends its password inside that TLS session, so the salamander
key riding in the same line becomes the only thing between an on-path peer and
the tunnel password. Prefer the REALITY link when the client can take it.

A client on either link negotiates its own congestion control, so it follows
neither the ladder nor `mole ceiling`.

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

### Address families

This machine's side is IPv4-only: the TUN carries no v6 address, the fake-IP
generator has no v6 range, and the DNS strategy is `ipv4_only`, so every AAAA is
answered empty. The kill switch refuses v6 egress on the physical interface
(link-local excepted), which is what makes that fail-closed rather than merely
absent — an app with its own resolver gets an immediate reset instead of a
SYN_SENT timeout, and falls back to v4 at once.

Which address a foreign site sees is decided by the **node**, because mole hands
domains across the leg and the VPS resolves them itself; its outbound is pinned
to `prefer_ipv6`. A dual-stack site is reached over the node's IPv6, an
IPv4-only one has no v6 address to be reached at and still leaves from the
node's IPv4 — so the exit is mixed by nature. Don't read `test-ipv6.com` as
contradicting this: it measures *this* machine's interfaces, where the v6 is
deliberately not, and will keep reporting none.

`prefer_ipv6` has no safety net under it. sing-box dials the first address the
resolver sorted out and does not try the next one when that fails, so a site
publishing a dead AAAA is unreachable through the tunnel rather than merely
slower. If a site only works with the preference removed, that is what is
happening — the knob is the node's `/etc/sing-box/config.json`, and it arrives
with every deploy.

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
