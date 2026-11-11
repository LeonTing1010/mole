# mole

Deploy a self-hosted REALITY + TCP Brutal VPN in **one command**. Self-healing,
auto-tuned, CN-direct by default. No subscription, no account, no third-party
ingress — you bring a VPS, mole brings everything else.

---

## 3-minute quick start

```bash
# 1. Install mole CLI (macOS)
go install github.com/LeonTing1010/mole@latest

# 2. Buy a VPS on Vultr, copy the API key, then:
export VULTR_API_KEY=...

# 3. One command creates + boots + verifies a full REALITY + hy2 tunnel
mole server deploy --name mole-tokyo --region nrt --plan vc2-1c-2gb

# 4. Connect your laptop — auto-tunes Brutal gain on first up
mole server use mole-tokyo
mole up --tune-on-start

# (opt) re-tune anytime after VPS reboot or IP change
mole server tune
```

That's it. ~3 minutes. The VPS runs sing-box v1.14.4 + hysteria2 v2.13.0 +
tcp-brutal v2.0.1 (gracefully falls back to BBR if your VPS lacks kernel headers).
Your client tunnel is self-healing — REALITY leg dies → auto failover to hy2 →
REALITY comes back → auto switch back. Brutal pacing auto-adjusts every tick.

## Deploying a server without the mole CLI

If you already have a VPS and just want to seed it with mole, use one of the
paths below. **Both produce a working sing-box + hy2 + Brutal keeper on port
443, and emit `/etc/mole/config.json` with the two client URIs.**

### Path 1: cloud-init (Vultr / Hetzner / DO — **recommended**)

```bash
# Vultr example:
vultr-cli instance create \
  --region lax --plan vc2-1c-2gb --os 26 \
  --label mole-server \
  --userdata "$(cat deploy/cloud-init.yaml)"

# Hetzner example:
hcloud server create --name mole --type cx22 --image ubuntu-24.04 \
  --user-data-from-file deploy/cloud-init.yaml
```

After boot (~3 min):

```bash
ssh root@<ip>
cat /etc/mole/config.json   # contains reality_uri and hy2_uri

# On your laptop, paste the reality_uri printed above:
mole server import 'vless://...' --name mole-reality
# Alternatively import hy2_uri as a separate profile:
# mole server import 'hysteria2://...' --name mole-hy2
mole up
```

Cloud-init and Docker share their config generator. All three deployment paths
share the same keeper and verified engine installers. After editing deployment assets, regenerate it with
`python3 scripts/render-cloud-init.py > deploy/cloud-init.yaml`.

Everything — sing-box, hy2, Brutal DKMS, Brutal keeper, self-signed cert,
REALITY keys, UUID, hy2 password — is generated on first boot. **No hardcoded
secrets in the cloud-init file.** Brutal DKMS is optional; the tunnel works
without it (falls back to BBR), so a VPS image missing kernel headers never
blocks REALITY.

### Path 2: Docker

```bash
# On any Docker host with kernel 6.1+ (tcp-brutal v2 requires it):
docker build -t mole-server deploy/

docker run -d --name mole-server --net host --privileged \
  -e MOLE_SERVER_IP=<public-server-ip> \
  -v /etc/mole:/etc/mole \
  -v /proc:/proc \
  mole-server

# Brutal DKMS still needs to be installed on the HOST (kernel module, not container):
sudo install -m 644 deploy/install-functions.sh /usr/local/lib/mole-install-functions.sh
sudo bash deploy/install-brutal.sh
```

Secrets are generated on first start and retained in the mounted `/etc/mole`.
Existing `REALITY_PRIV`, `REALITY_SHORT`, `USER_UUID`, and `HY2_PASSWORD` environment
values are supported; a conflicting value on restart is rejected. Client links are
in `/etc/mole/config.json`, mode 0600. The keeper listens only on loopback.
Docker requires host-installed TCP Brutal for pacing; both transports work without it.

For manual installation on Debian/Ubuntu, run `sudo bash scripts/mole-server.sh`
from this checkout. `MOLE_REVISION` selects the source revision to download;
`MOLE_SERVER_IP` supplies the public IP when automatic discovery is unavailable.

### Path 3: mole CLI (full lifecycle)

```bash
mole server deploy              # create VPS, verify, record
mole server ls                  # list deployed (alias: list)
mole server verify <name>       # re-check without repairing
mole server destroy <name>      # delete VPS + local record
mole server sync                # diff local list vs Vultr API (alias: reconcile)
mole server adopt <instance-id> --password <hy2> # adopt a Vultr instance
mole server import <uri>        # import a hysteria2:// or REALITY vless:// link
mole server link [name]         # print vless:// link for other clients
mole server tune [name]         # auto-scan Brutal cwnd_gain (alias: gain-tune)
                                # 8-concurrent 8MB bulk — picks best for REAL workload
```

---

## Core capabilities

### Dual-leg transport with automatic failover

A mole VPS runs two transports on the same port: **REALITY (VLESS + XRay TLS
reality)** and **Hysteria2**. The client defaults to REALITY (TCP, masquerades
as legitimate HTTPS); if REALITY fails three spaced fresh-dial checks AND an
out-of-tunnel HTTPS probe confirms path death, the supervisor fails over to
hy2 — zero reconnect, zero TUN tear-down, zero outage in the tunnel. When
REALITY recovers it auto-switches back.

```bash
mole leg auto        # REALITY primary, hy2 fallback (default)
mole leg reality     # pin to REALITY only
mole leg hy2         # pin to hy2 only
mole leg both        # split: 6 REALITY ports + 2 hy2 ports, auto-balance
```

### TCP Brutal congestion control

Both legs use [tcp-brutal](https://github.com/apernet/tcp-brutal) v2.0.1 — a
kernel-level congestion control that paces every TCP toward your client at a
declared rate. Measured +24–60% throughput on lossy CN→US paths vs BBR, and
*prevents the flood collapse* where a congested REALITY leg buffers 1000+
packets and the tunnel hangs.

```bash
mole ceiling              # show current rung + auto profile
mole ceiling 40           # pin to 40 Mbps (no reconnect — Clash API call)
mole ceiling auto         # back to time-of-day schedule

mole server tune [name]    # scan gain ∈ {10,15,20,25} with 8-concurrent 8MB
                          # bulk at each; writes best gain to servers.json
                          # AND applies live to the keeper immediately
mole up --tune-on-start   # auto-tune on first up (when brutal_gain==0)
```

`BrutalGain` is per-IP — the same rung works differently on different transit
paths. `mole server tune` (alias: `gain-tune`) is the only way to pick the right one.
Measured on a good Vultr IP, default gain=20 (aggressive) was 2× faster than
gain=15; on a TransitWireless IP, gain=10 was 3× the concurrent throughput of
gain=20.

### The declared rate is not adaptive — unless you ask it to be

TCP Brutal paces at exactly the rate it is told and never probes for more. That
is why it beats BBR on long, lossy paths: no slow-start climb, and path loss is
not mistaken for congestion. The price is that a wrong rate is never
self-corrected, and the bottleneck is usually the side the node cannot see —
Wi-Fi, a saturated uplink, a tethering quota. Declare 600 Mbps over a 30 Mbps
Wi-Fi link and Brutal will keep flooding it; single streams collapse to a
fraction of the link.

Set `reality_rate_auto` on the server entry to hand the declared rate to a
client-side governor instead:

```json
{ "name": "icn-...", "reality_down_mbps": 35, "reality_rate_auto": true }
```

`reality_down_mbps` then seeds the loop, and the peak profile is its ceiling.
Every third supervisor tick the client measures what the tunnel actually
delivers (8 concurrent streams, burst sized to about one second at the declared
rate, capped at 6 MB) and moves the declaration:

| Measured vs declared | Action |
|---|---|
| < 50% | cut to 25% of the declaration, floored at 1.5× measured |
| 50–80%, two samples in a row | cut 15% |
| 80–95% | hold |
| ≥ 95%, one sample, below 75% of the ceiling | climb 35% |
| ≥ 95%, two samples in a row | climb 12% |

The asymmetry is deliberate: a collapsed link is felt immediately and is fixed
on one sample, while jitter and transient lulls need corroboration so the loop
does not chase noise in either direction. Climbing gets the same treatment from
the other side — a probe taken at the declared rate can never measure more than
that rate, so the loop has to overshoot on purpose to find headroom, and it
overshoots in bigger steps when it is far below the ceiling. Without those big
steps, recovering from a collapsed declaration on improved Wi-Fi would take
hours of millimetric climbing.

There is one thing no band handles well: a discrete jump. Moving from a 2.4GHz
roam to 5GHz, or swapping Wi-Fi for a cable, changes the path by an order of
magnitude, and the old declaration says nothing about the new one. The loop
watches the radio's channel and band — not transmit rate or signal, which drift
constantly — and on a change it re-declares the peak, then treats the very next
measurement as authoritative instead of waiting an interval. That single
overshoot is visible in the log as `local link changed ... re-declaring`.

Off by default — leaving `reality_rate_auto` unset keeps `reality_down_mbps` a
fixed pin.

### Self-healing supervisor

Once `mole up` the daemon runs a scheduler that:

| Tick | Checks | Action |
|---|---|---|
| 1 min | Link probe (UDP + ICMP) | ↓ rung if path dies, ↑ rung when clean (15-min uphill cooldown) |
| 3 min | ENOBUFS flood counter | pause hy2 allocations, descend one rung |
| 10 min | REALITY Brutal keeper still responding? | re-push rate; 3 unconfirmed → `brutalUnpaced` flag |
| Continuous | REALITY leg health | 3 fails → auto failover to hy2; 3 succeeds → return to primary |

Everything is reflected in `mole status`. Nothing auto-destroys your VPS —
destroy is a lid-close option, a CLI command, or a reconcile action.

### CN-direct routing by default

All traffic is split at the TUN boundary:

- **China IPs** (`geoip-cn.srs`) → **direct** interface (no tunnel, no VPS)
- **Foreign IPs** → through the VPS tunnel
- **Chinese `.com` sites** (baidu, bilibili, xiaohongshu, aliyun, …) → real
  IP via AliDNS (`geosite-cn.srs`), so they route direct *not* to the VPS

DNS uses sing-box's **fake-IP generator** for foreign A queries — no upstream
dependency. If your VPS dies, foreign sites hang (as designed) but domestic
sites keep working.

### Kill switch (macOS)

PF anchor `com.apple/mole` is armed **before** sing-box starts. Physical
interface egress is blocked except for the VPS endpoint, China IPv4 space,
LAN, and DHCP. If sing-box dies, foreign connections **block** (fail-closed)
rather than silently leaking. `mole down` disarms it.

### Fake-IP persistence

DNS metadata (`~/.mole/sing-box-cache.db`, mode 0600) survives restarts so
cached application mappings don't force fresh fake-IP allocations and connection
drops on engine restart.

---

## CLI quick reference

```
mole up          [--no-killswitch] [--no-destroy-on-lid-close]   # bring tunnel up (background)
mole down        [--destroy]                                      # tear down; optionally destroy VPS
mole status                                                       # server, leg, rung, health, exit IP
mole ceiling     [mbps|auto]                                      # show or pin bandwidth rung
mole leg         [auto|reality|hy2|both]                          # show or pin leg mode
mole reload                                                       # reload custom-rules.json + hotrule-sets

mole server deploy    [--name X --region Y --plan Z --no-reality --no-hy2]
mole server destroy   <name>
mole server ls
mole server sync      [--destroy-remote]
mole server verify    <name> [--timeout 6m]
mole server adopt     <reality-uri> [<hy2-uri>]
mole server import    <config.json>
mole server link      [name] [--hy2]
mole server tune [name]
```

---

## What's inside

| Layer | Component | Role |
|---|---|---|
| Client | sing-box **v1.14.4** | TUN + routing + DNS + REALITY/hy2 outbound + Clash API |
| Client | **smux** (not yamux!) | Session pooling — 8 connections share one TCP, ~82% faster |
| Server | sing-box v1.14.4 | REALITY inbound (VLESS + XRay reality) |
| Server | hysteria2 **v2.13.0** | Hy2 inbound on same port + TLS cert |
| Server | TCP Brutal **v2.0.1** | Kernel module, cwnd_gain tuned per-IP |
| Server | Brutal keeper | Python (stdlib) on :8098 — in-band rate push + bulk test + health |
| Client | mole supervisor | Scheduler: ceiling pin/clock, leg pin, failover, self-heal, keeper confirm |

Pinned versions + SHA-256 digests in `cmd/nodeinstall.go` and `cmd/brutal.go`.
Upgrading requires updating both. Version qualification before use: mole
flags new sing-box minor releases as `unverified` on a fresh VPS.

### Architecture

```
┌─ Laptop ─────────────────────────────────────────────┐
│ mole daemon (supervisor)                              │
│  ├── sing-box TUN (gVisor stack, auto_route=1, strict)│
│  ├── Clash API (selector flip, no reconnect)          │
│  ├── Brutal rate push → forwarder → keeper            │
│  └── keepalive + self-healing                         │
│                                                       │
│  REALITY outbound ────────── TCP/443 ──┐              │
│  hy2 ladder (selector × 8) ─ QUIC/443 ─┤              │
└────────────────────────────────────────┼──────────────┘
                                         │
                                    VPS /443
                                         │
              ┌──────────────────────────┼──────────────────────────┐
              │                          │                          │
    REALITY inbound              hy2 inbound              Brutal keeper
    (VLESS + reality)         (password + salamander)    (:8098 loopback)
              │                          │                          │
              └──────────────────────────┴──────────────────────────┘
                                         │
                                    kernel 6.1+
                                         │
                                  tcp-brutal DKMS
                                  rate + cwnd_gain per-peer
```

---

## Requirements

| | Client (macOS) | VPS |
|---|---|---|
| OS | macOS 13+ | Ubuntu 22.04+ (Debian bookworm also works) |
| Runtime | sing-box v1.14.x | sing-box v1.14.4 + hysteria2 v2.13.0 |
| Privilege | root (`mole up` sudo-execs) | — |
| Outbound | — | port 443/tcp + 443/udp open |
| Kernel | 6.0+ for Brutal (graceful skip) | 6.1+ strongly recommended |

## Testing

```bash
go test ./...              # all packages; live tests need a mole tunnel up
go test ./core -v -run Leg  # specific sub-suite
```

37% test coverage (10k of 27k lines). Live tests measure real bulk throughput,
real ENOBUFS pressure, real keeper confirmations. No mocks for transport paths.

## Status

- **Sing-box** v1.14.4 ✓ qualified
- **Hysteria2** v2.13.0 ✓ qualified
- **TCP Brutal** v2.0.1 ✓ qualified (with cwnd_gain tuning, gain=20 default, per-IP override via `server tune`)
- **cloud-init.yaml** ✓ — Vultr / Hetzner confirmed boot
- **Dockerfile** ✓ — local build + run tested

## License

MIT
