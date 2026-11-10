# Network recovery and performance verification — 2026-11-03

The six audited fixes were integrated and the installed daemon was restarted.
Only the named source deltas were copied from the isolated checkout. Existing
unrelated dirty work was preserved. The previous executable is retained for rollback.

| Check | Before | After |
| --- | --- | --- |
| Broken HY2 fallback with recovered REALITY | State cleared while selector stayed on HY2 | Three spaced candidate checks, then actual selector committed |
| Cloudflare 503 with another valid origin | False transport fallback | REALITY retained |
| Optional TCP pacing unavailable | Candidate recovery suppressed | Authenticated recovery measured; capability reported separately |
| Concurrent state writes, 288 attempts | 203 failed | 0 failed; final JSON readable |
| Application FakeIP reused after real engine restart | Connection failed, missing mapping | PONG delivered without another DNS lookup |
| DNS metadata permissions | Engine default 0644 | Save creates or tightens to 0600 without truncation |

The state-write stress is a deterministic failure reproduction, not an online
failure rate. Recovery fault injection tests use mocked transport and selector
boundaries. FakeIP restart uses the real installed sing-box 1.14.3 with production
sniff behavior and plain application TCP, so SNI cannot mask a missing mapping.

Go race tests across all packages, six Python tests, vet, diff checks, the real
engine generated-config check, and the release build passed. The build from the
integrated shared checkout matches the installed executable. See `gates.log`,
`red.log`, `green.log`, and the separate cache privacy RED/GREEN evidence.

## Performance decision

HY2 PMTUD A/B used the same current node, TLS pin, obfuscation, physical
interface and declared 400 Mbps rate. Three alternating-order rounds delivered
all 480 MiB with no failed phase or new ENOBUFS. All 22 HTTPS checks passed.

| Total concurrent flows | Fixed 1200 median | PMTUD median |
| --- | ---: | ---: |
| 1 | 118.66 Mbps | 117.95 Mbps |
| 8 | 187.52 Mbps | 202.48 Mbps |

The eight-flow paired rounds changed by about +8%, -4%, and +27%; this small,
variable sample does not establish a reliable gain. Native forwarders bypass
TUN. Preserve the conservative default rather than promote this candidate.

An administrator-authenticated separate utun30 with `auto_route:false` and an
interface-scoped default route exercised real TUN traffic while the production
VPN remained active. gVisor delivered 103.1 Mbps single-flow and 212.4 Mbps over
eight flows in the pilot. mixed returned empty responses in that harness,
including after listener settle and retry, so no valid speed comparison exists.
This does not establish how mixed behaves under ordinary automatic routing.
All owned test routes/interfaces were removed and the production configuration
hash was unchanged during these tests. gVisor remains the default.

Deployment installer parallelism from the preceding change is retained. No new
billable node was created in this run, so no real deployment speed percentage
is claimed. Unsupported server root `mtu` output was removed for future scripts;
current client packet defaults were preserved.

## Activation and boundaries

The final daemon maps the inode of the verified installed executable. PF is
checked in the kernel, alongside the armed lid policy and mode-0600 cache.
HY2 and REALITY both passed authenticated external HTTPS verification. The
actual system TUN passed 20/20 fresh TLS checks across two origins. Cloudflare
exit matching uses the node's observed IPv6, rather than incorrectly assuming
it must equal the node's IPv4 listener. No new timeout, missing-FakeIP or
ENOBUFS message appeared during this check.

The live monitor is still collecting its post-restart sample window. Short
checks do not establish long-term stability or sustained Internet capacity;
no throughput improvement is claimed for these recovery fixes. No upload,
wired connection, remote server failure or bulk transfer without TCP pacing
was qualified. Lower monitor latency can reflect the new target set.

Machine-readable provenance, source manifest, backup path and exact hashes:
`receipt.json`. Runtime evidence: `activation.json`, `root-readback.json`,
`postactivation.json`, and `server-verify.log`. No production credentials or
raw production configuration are included in these artifacts.
