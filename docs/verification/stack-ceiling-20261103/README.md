# Stack maintenance and measured 600 Mbps REALITY download ceiling

Source fixes are integrated and installed. The active root daemon maps the new
Mole binary, built using Go 1.26.10, and the privately installed stable sing-box
1.14.4. Both loaded inode/hash checks passed. No commit was created.

## Measured bandwidth choice

Same node, IPv4 REALITY, Vision + smux 8/1, stable 1.14.4, gVisor MTU9000,
unchanged OS controls. Three rotated rounds, equal 64 MiB counted warmup,
32 MiB single-flow and 128 MiB eight-flow measurements per condition.
Concurrent TLS requests measured responsiveness; every requested bulk byte was
counted. No builds or tests ran during the confirmation series.

| Download pacing | Single-flow median | Eight-flow median | Eight-flow TLS median |
|---|---:|---:|---:|
| 400 Mbps | 181.1 Mbps | 378.2 Mbps | 263 ms |
| 500 Mbps | 175.1 Mbps | 361.2 Mbps | 445 ms |
| 600 Mbps | 178.1 Mbps | 474.8 Mbps | 318 ms |

The 600 condition exceeded 400 in all three paired rounds, with a median
25.6% increase. It delivered 474.8 / 552.5 / 430.1 Mbps; the declaration
is a pacing target, not a throughput guarantee. 85/85 concurrent TLS checks
passed across all confirmation phases, and all 2016 MiB were delivered, with
zero ENOBUFS. Rules were restored to400 after each isolated experiment.
The initial pilot (`ceiling.json`) had a smaller 32 MiB warmup and some concurrent
small test builds; its different ranking did not determine the final setting.
500 was not a repeatable improvement. Single-flow speed did not improve.

Only the active node's `reality_down_mbps` changed from400 to600. HY2 fallback,
upload, and peak/off-peak ladder remain400. This uses the existing independent
REALITY pacing mechanism. Changing it does not require advertising an untested
600 Mbps upload or QUIC ceiling. The supervisor has read it back as600/confirmed.

## Source changes and why

- `cmd/brutal.go`: parse native IPv6, bracketed/unbracketed ss endpoints, and
  mapped IPv4; validate IPs and ports, deduplicate mapped addresses, use exact
  /32 or /128 policies. The regression reproduces the old omitted IPv6 peers
  and malformed IPv4 acceptance, then captures actual deployed Python calls.
- `core/engine_contract.go`, `cmd/up.go`, `core/singbox.go`: qualify stable
  1.14.x >=1.14.3 and check the generated config before DNS/sysctl/PF takeover
  and before each engine launch. Reject unqualified minor/prerelease versions.
  Remove emitted `dns.independent_cache`; retain the qualified gVisor stack.
- `core/fakeip_recovery.go`: 1.14 consumes the persisted allocator metadata on
  startup and debounces later updates. A forced death can leave committed
  mappings with missing/stale metadata; the next engine would reset them or
  allocate an already used address. Before launch, exclusively lock the
  offline DB for at most250ms, validate the expected bucket/record format and
  reverse mappings, reconstruct/advance the allocator to committed addresses,
  and save a private pre-repair backup before a single transactional update.
  Preserve existing mappings and unrelated buckets. Keep at most3 recovery
  backups per DB. Live locks, ambiguous records, and malformed metadata are
  refused without destructive resetting. Explicit range changes with valid
  old metadata retain the engine's migration behavior.
- Pin the same SagerNet bbolt module as stable1.14.4 for this offline adapter;
  its dependency resolution also updates pflag1.0.9 to1.0.10. The official packet
  engine is unmodified; no private engine fork is required.

## Acceptance and activated state

`final-gates.log`: Go1.26.10, all Go race tests, six Python tests, vet,
`git diff --check`, and production build passed. The earlier complete gate
using the original compiler also passed. Real-engine FakeIP tests cover fresh,
mature/graceful, and stale checkpoints; repeated SIGKILL with no new DNS
query still delivers plain non-SNI PING/PONG on old cached addresses, and new
allocation does not reuse them. Unit negatives cover locked/ambiguous/range
mismatch/corrupt metadata, IPv6 allocation, and bounded private backup retention.
This does not assert recovery from filesystem corruption or power loss.

Automatic reconnect succeeded without rollback. Root readback confirms active
CLI and engine match installed inodes, PF enabled with80 rules including the
node exemption, cache0600, and destroy-on-lid-close armed. Generated packet
configuration differs only by removal of the redundant DNS field; direct routes
and packet settings are otherwise identical.

A post-activation production-TUN test delivered exactly1GiB across eight
application flows in20.85s (412.1Mbps),
with43/43 authenticated TLS checks during load and no new ENOBUFS, timeouts,
or missing FakeIP. Clash connections show proxy-reality/proxy/flow routing;
the FakeIP address traversed the production utun4. This sustained observation
uses a different duration/budget from the short comparison and is not the
same estimator as its median. A further20/20 actual-TUN HTTPS checks passed.
`mole server verify` authenticated both HY2 and REALITY from outside the tunnel
and delivered HTTPS payloads, then recorded ready.

## Deployment and rollback boundary

The IPv6 keeper source is fixed and verified for future deploy/repair. Updating
the current node was attempted through a fresh private SOCKS endpoint using the
original authenticated REALITY identity to node loopback SSH, without opening
cloud ingress. SSH reached the node but rejected the local key; no remote file,
service, or firewall rule was changed. The running node's engine/module versions
remain unobserved. Current IPv4 transport already has applicable pacing and the
600 setting is confirmed independently of that pending IPv6 activation.

Rollback artifacts are at `/Users/leo/.mole/changes/stack-ceiling-20261103-102833`. The old CLI, packet config, and
server store are preserved. Homebrew's prior1.14.3 engine remains intact; remove
the task's private1.14.4 engine and restore the old CLI/400 override before a
normal reconnect to roll back. Credentials are not included in this report.
Unrelated dirty work was protected by baseline file hashes and exact-path copy.

Primary implementation references: [stable1.14.4](https://github.com/SagerNet/sing-box/releases/tag/v1.14.4),
[FakeIP storage](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.4/experimental/cachefile/fakeip.go),
[allocator](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.4/dns/transport/fakeip/store.go),
[TCP Brutal v2 IPv6/rule support](https://raw.githubusercontent.com/HyNetworks/tcp-brutal/v2.0.1/brutal_rules.c).
