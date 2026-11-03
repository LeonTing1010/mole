# Stack maintenance and download ceiling measurements — 2026-11-03

A private production profile now uses a 600 Mbps REALITY download pacing target.
This is a measured per-node setting, not a new repository-wide default. HY2
fallback and upload targets remain 400 Mbps on that profile. Direct routing was
unchanged. The active local controller was built using Go 1.26.10 and the loaded
packet engine was verified as official stable sing-box 1.14.4, including inode
and SHA-256 readback after reconnect.

## Controlled comparison

Same node and IPv4 transport, REALITY with Vision and smux 8/1, sing-box 1.14.4,
gVisor, MTU 9000, unchanged OS controls. Three rounds used rotated ordering.
Each condition had an equal counted 64 MiB warmup, a 32 MiB single-flow sample,
and a 128 MiB eight-flow sample through an isolated full TUN. Concurrent HTTPS
requests tested responsiveness. No builds or tests ran during confirmation.

| Pacing target | Single-flow median | Eight-flow median | Eight-flow HTTPS median |
|---|---:|---:|---:|
| 400 Mbps | 181.1 Mbps | 378.2 Mbps | 263 ms |
| 500 Mbps | 175.1 Mbps | 361.2 Mbps | 445 ms |
| 600 Mbps | 178.1 Mbps | 474.8 Mbps | 318 ms |

The 600 Mbps condition delivered 474.8, 552.5 and 430.1 Mbps across the three
rounds, exceeding the paired 400 Mbps condition in every round. Its median was
25.6% higher. All 2016 MiB were received; 85/85 concurrent HTTPS checks passed,
with no ENOBUFS. The 400 Mbps policy was restored after each isolated condition.
An earlier pilot had a smaller warmup and some concurrent test compilation;
its different ranking was not used to choose the final setting. The 500 Mbps
target did not provide a repeatable benefit. Single-flow speed did not improve.

After activation, eight application flows through the actual production TUN
received exactly 1 GiB in 20.85 seconds, averaging 412.1 Mbps. All 43 concurrent
HTTPS requests passed, with no new timeouts, ENOBUFS or missing FakeIP records.
A further 20/20 production-TUN HTTPS checks and independent authenticated HY2
and REALITY HTTPS payload checks passed. PF rules and lid-close policy remained
armed. These are point-in-time observations; the longer production sample uses
a different duration and byte budget from the short comparison. A pacing target
is not a guaranteed Internet download rate.

## Source fixes and qualification

- Deploy installs HY2 and REALITY independently under a shared bounded deadline,
  with pinned, hash-verified artifacts. Optional TCP Brutal cannot block required
  protocol readiness. Only authenticated external HTTPS delivery establishes
  readiness; IP allocation and process state do not establish it.
- Warm/fresh monitors validate two independent origins. Recovery preserves its
  evidence until the actual selector commits; optional pacing confirmation is
  reported independently of successful transport delivery.
- Engine version and config checks run before DNS/sysctl/PF takeover and before
  engine launch. Current schema/cache handling qualifies stable 1.14.x >=1.14.3;
  new minor or prerelease versions require qualification. The redundant
  `dns.independent_cache` option is no longer emitted. The qualified gVisor stack
  remains in use.
- FakeIP recovery reconciles committed mappings with missing/stale allocator
  metadata while the engine is offline. It takes an exclusive bounded lock,
  validates forward/reverse records, preserves a mode-0600 pre-repair snapshot,
  and changes the checkpoint transactionally. It retains at most three backups.
  Unknown formats, inconsistent mappings and live locks are refused without
  resetting the cache. Explicit range migrations keep the engine's behavior.
- IPv6 TCP pacing parses native IPv6 and mapped IPv4 peers, validates endpoints,
  deduplicates mapped addresses and emits exact `/128` or `/32` policies.

Go race tests across all packages, six Python tests, vet, diff checks and the
production build passed with Go 1.26.10. Real-engine FakeIP acceptance covered
fresh, mature/graceful and stale checkpoints, repeated SIGKILL without a new DNS
query, plain non-SNI application payloads on old cached addresses, and collision
checks on new allocations. Negative cases cover locked, inconsistent and invalid
metadata, IPv6 allocation, and bounded private snapshot retention. This does not
assert recovery from filesystem corruption, power loss or exhausted cache ranges.

The IPv6 keeper source is fixed. Deployment to the existing node was blocked by
SSH key rejection; no remote file or firewall rule was changed. Current IPv4
pacing was independently confirmed at 600 Mbps. Running remote engine/module
versions remain unobserved. The new local official engine is unmodified.

References: [sing-box 1.14.4](https://github.com/SagerNet/sing-box/releases/tag/v1.14.4),
[FakeIP storage](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.4/experimental/cachefile/fakeip.go),
[allocator](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.4/dns/transport/fakeip/store.go),
[TCP Brutal IPv6/rules](https://raw.githubusercontent.com/HyNetworks/tcp-brutal/v2.0.1/brutal_rules.c).

Machine-specific receipts, rollback locations, cloud identifiers and raw local
logs are retained locally and are not part of this publication.
