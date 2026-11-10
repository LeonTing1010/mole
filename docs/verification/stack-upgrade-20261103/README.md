# Mole full-stack upgrade assessment — 2026-11-03

The useful immediate maintenance candidate is local sing-box 1.14.4. Its verified
release binary passed the existing project gates and both authenticated protocols.
It did not establish a throughput improvement. The 1.15 alpha native Go TUN stack
reduced CPU time but requires configuration migration and is not recommended as
the production engine. No production binary, node, route or connection was changed.

## Runtime, release and dependency inventory

| Layer | Observed / source-pinned state | Upgrade judgment |
| --- | --- | --- |
| Hardware / macOS | M5, 16 GiB, macOS 27.2; active Wi-Fi, no active Ethernet | The OS updater offered only Xcode Command Line Tools 27.2. No OS upgrade was offered. Prior OS tuning tests found no repeatable speed gain. |
| Mole controller | Installed executable built with Go 1.26.2 | Maintain on Go 1.26.10 or qualify 1.27.3. Mole orchestrates the external engine; replacing its build SDK does not replace the packet-processing runtime. |
| Local packet engine | sing-box 1.14.3, built with Go 1.26.9 | Stable 1.14.4 is available and qualified below. It is built with Go 1.26.10. |
| TUN | Current gVisor stack, MTU 9000 | 1.14.4 updates sing-tun; 1.15 alpha defaults to sing-tun's own Go stack. The latter is a measured CPU candidate, not an established speed gain. |
| REALITY / TLS / smux | Vision + smux 8/1, uTLS 1.8.7 | 1.14.4 has a Vision close-path fix. smux and sing-mux dependency pins are unchanged across the compared engine versions. |
| Client QUIC / HY2 | SagerNet quic-go 0.61 fork, mod.9 | 1.14.4 brings mod.12 and a newer sing-quic snapshot. Use the engine's tested dependency set. |
| Node sing-box | Current installer pins 1.14.4 | Running node version was not read back; installer source is not runtime attestation. Do not reinstall an assumed-old server. |
| Node Hysteria | Installer pins 2.13.0; latest upstream release is 2.13.0 | No newer stable release was found. Native Hysteria's apernet/quic-go fork differs from the sing-box client fork; their version numbers are not a drop-in upgrade recipe. |
| Node TCP Brutal | Installer pins 2.0.1; latest upstream release is 2.0.1 | No newer stable release was found. Mole's IPv6 peer handling has a gap independent of the module version. |
| Node OS / kernel | Deployment requests Ubuntu 24.04 | Actual running kernel, package versions, CPU/steal and per-socket congestion control remain unobserved. There is no capacity evidence for a kernel or VPS-size upgrade. |

Source pins are in `cmd/nodeinstall.go` and `cmd/brutal.go`. The current data engine
is a separate executable selected by `core/singbox.go`, preferring
`~/.mole/bin/sing-box` before PATH. Installing or updating a different binary does
not prove which engine a live root daemon loaded.

Parent-module lookups also found newer pflag, x/crypto and x/sys versions. Exact
current/latest versions are in `mole-dependency-versions.json`. Their maintenance
review is separate from throughput: changing Mole's go.mod does not update the
libraries compiled into the external sing-box executable. The two newer x/*
modules inspected require Go 1.26; no minimum-Go conflict was established.

## Stable changes relevant to this deployment

Release notes are terse, so the audit traced the release comparison and dependency
changes. Relevant 1.14.4 paths include:

- QUIC/UDP EMSGSIZE handling and application-limited detection changes.
- VLESS Vision direct-write close handling.
- Accepted TUN flow entry cleanup and closed system-stack TCP session expiry.
- Session-based outbound warmup and connection-tracker close races.
- Updated Go patch runtime and transport-library snapshots.

These are maintenance/reliability reasons to upgrade, not a guaranteed throughput
percentage. EMSGSIZE is not ENOBUFS. Fixing the former does not establish that the
Mac's earlier buffer-exhaustion failure is solved. System-stack fixes do not all
apply to the currently selected gVisor path, and Chrome QUIC-parrot changes are
not evidence of a gain for the current Salamander/Brutal profile.

Selected upstream commits for review:

- [QUIC/UDP EMSGSIZE](https://github.com/SagerNet/sing-box/commit/af288b53e6ebdf1014def2fadd7ed1cc4e7a0613)
- [Vision close path](https://github.com/SagerNet/sing-box/commit/770d047aeb5fc80bbc60208c0544a7ff0f1b4e91)
- [TUN accepted-flow cleanup](https://github.com/SagerNet/sing-box/commit/03ab009ea1c0d7c8d9cba6114ca4a65cda6ceea7)
- [System TCP session expiry](https://github.com/SagerNet/sing-box/commit/cc2f91b77a6964c7b85c5f94f4035eb41555a313)

## Same-node full-TUN upgrade measurement

Official Darwin/arm64 candidate archives were verified against the publisher's
release-asset sizes and SHA-256 digests before any candidate execution. Exact
archive and binary hashes are in `candidate-binaries.json`.

All versions used the same owned node, IPv4 endpoint, REALITY credentials,
certificate behavior, smux 8/1, 400 Mbps declaration, Wi-Fi and MTU 9000. The alpha
case omitted the legacy `stack` field to select its new default implementation.
Each isolated engine validated external TLS and downloaded a counted 64 MiB warmup
before the measured 32 MiB single-flow and 128 MiB eight-flow phases. Three rounds
rotated ordering. Only owned FakeIP host routes used the temporary utun30; the
production default route and DNS settings remained untouched.

| Engine | Single-flow median | Eight-flow median | Engine CPU time for 128 MiB, median |
| --- | ---: | ---: | ---: |
| Current 1.14.3 / gVisor | 181.6 Mbps | 378.5 Mbps | 1.40 s |
| Stable 1.14.4 / gVisor | 186.4 Mbps | 378.3 Mbps | 1.45 s |
| 1.15.0-alpha.13 / Go TUN | 177.3 Mbps | 378.7 Mbps | 0.97 s |

All 27 phases, including warmups, delivered their full budgets: 2016 MiB. No new
production ENOBUFS appeared. Eight-flow throughput medians are effectively equal.
The alpha used about 31% less CPU time than the current artifact and 33% less than
the stable candidate. This is process CPU time, not measured battery-life gain.
Single-flow differences did not establish an upgrade acceleration.

The old engine also reached roughly 378 Mbps after identical warmup. Earlier
cold/short measurements near 210 Mbps cannot be treated as the baseline for an
upgrade gain. The current result is near the declared 400 Mbps setting, but that
ratio alone does not establish the only bottleneck. This measures the encrypted
Mac-to-node path and owned-node payload, not arbitrary website capacity or upload.

The comparison qualifies actual release artifacts. Compiler patches and bundled
libraries also differ; it is not a one-line source-change causal experiment.

## Configuration and protocol compatibility

1.14.4 accepted the generated production-style configuration with an isolated
private cache path. The existing project verification script, using the candidate
engine in the test PATH and an isolated HOME with real rule-set fixtures, passed:
Go race tests across all packages, six Python tests, vet, diff checks and build.
The resulting Mole executable is byte-identical to the installed controller.
See `stable-gates.log`.

The alpha rejected the current `dns.independent_cache` option. Removing that
field and `tun.stack` produced a valid configuration. The isolated native-stack
measurements used the migrated form. Existing startup environment flags do not
supply the newly required legacy-DNS override, so it is not a binary-only upgrade.
Proper migration is preferred to depending on a legacy-feature override.

For every engine, a separate private Clash API / SOCKS client successfully:

1. Read the API version.
2. Selected REALITY and read back the actual selector.
3. Authenticated external HTTPS and delivered 8 MiB over that leg.
4. Repeated selection, HTTPS and 8 MiB payload over HY2.

These checks preserve the original TLS pins and obfuscation. Both protocol paths
passed for all versions. No source pin implies that the remote running engine
version was observed; wire compatibility and installed-version attestation are
different claims.

## Restart, upgrade and rollback boundary

Real engine tests used a fresh private DB, production-style sniff behavior and
plain application TCP, so SNI cannot hide missing FakeIP records. All three
versions recovered the cached application address after graceful restart.
Four graceful cache migration/rollback cases also passed:

- current to stable, and stable to current;
- stable to alpha, and alpha to stable.

Fresh-cache forced-kill tests failed on all versions, both immediately and after
a two-second delay. Before each kill the same address delivered PONG; after
restart without another DNS query it returned SOCKS failure. This is a remaining
recovery boundary, not an alpha-only regression and not proof that every mapping
in a mature production DB is lost. The previous Mole verification established
normal/graceful restart recovery; it did not establish recovery after arbitrary
process death.

`core/singbox.go` already attempts a graceful stop before a bounded SIGKILL
fallback. Upstream upgrades alone did not close the new forced-crash reproduction.
A later Mole change should explicitly qualify stale client DNS after forced
recovery, including mature-cache cases and actual application behavior.

## Mole gaps with more direct relevance than version chasing

### IPv6 pacing coverage

`cmd/brutal.go:155` extracts only IPv4 peers and line 193 appends `/32`. Upstream
TCP Brutal 2.0.1 already supports IPv6 prefix parsing and IPv6 routing. Therefore
upgrading that module does not fix the keeper's omission. Add correctly parsed
IPv4/IPv6 peers and `/32` or `/128` rules, then compare the two paths under matched
pacing. This is a performance hypothesis requiring deployment and measurement;
no speed gain has been established, and no production change was made here.

This also bounds the previous IPv4/IPv6 experiment: current IPv6 configurations
were slower, but the result cannot be wholly attributed to routing quality when
the peer pacing treatment is unequal. Node-to-website preferred IPv6 egress and
Mac-to-node IPv6 transport are separate legs.

### Pacing-rule confirmation versus a live socket

The keeper discovers established connections before installing rules. Upstream
v2 destination rules are selected when a TCP connection is established. On an
initially missing rule, a later policy installation does not prove that an
already-open connection adopted Brutal. A future lifecycle change can qualify
fresh data connections after policy confirmation or obtain per-socket evidence.
This is a source-level boundary; the current 378 Mbps tests do not prove an
active production pacing fault.

### Engine configuration contract

The engine search path has no version-based configuration migration gate. Before
a future 1.15 stable adoption, qualify version/capability detection, both schema
changes, direct/proxy routing, DNS persistence, selector APIs and recovery. Avoid
assuming a package-manager version is the active engine or that a new executable
can consume every old configuration unchanged.

## Recommended sequence

1. Adopt verified stable local sing-box 1.14.4 as maintenance. Preserve the old
   engine and verify the loaded path/version after restart. Expect reliability
   fixes; do not promise additional Mbps from these measurements.
2. Qualify Mole's IPv6 peer pacing and forced-crash DNS recovery. These address
   demonstrated application gaps. Only a matched post-fix path comparison can
   establish a speed benefit.
3. Maintain the controller's Go patch level and libraries through their tests.
   They are separate from the runtime of the prebuilt packet engine.
4. Revisit the 1.15 Go TUN stack when stable and after full configuration/lifecycle
   migration. Its measured CPU advantage is useful evidence; the alpha status,
   migration requirement and unchanged throughput do not justify replacing the
   current production engine for speed alone.

No server kernel, global congestion controller, protocol replacement, VPS tier
or operating-system upgrade is justified by the observed capacity evidence.
TCP Brutal's rules select congestion control for matched node-to-client sockets;
changing the server's default BBR version does not automatically change that path.
MPTCP or L3 forwarding would require an appropriate topology/protocol path and
have not been qualified as drop-in accelerators for this VLESS/HY2 setup.

## Runtime preservation and evidence

The live daemon remained PID 21648, with the original installed engine and
configuration hashes, armed lid policy and mode-0600 cache. Final actual TUN
HTTPS checks passed 20/20 and observed no new timeout, ENOBUFS or missing-FakeIP
message. This task staged and tested candidates; it did not install an upgrade,
restart production or modify direct-routing policy.

`receipt.json` contains machine-readable results, dependency pins, source
locations and limitations. Candidate test scripts read credentials from the
existing private configuration and stage their own protected test files. No
production credential or full production configuration is included here.
Reproduction requires the exact verified candidate binaries at the staged paths
listed in the receipt; the artifacts do not ship third-party executable payloads.

## Primary release and implementation references

- [sing-box 1.14.4](https://github.com/SagerNet/sing-box/releases/tag/v1.14.4), [1.15 alpha](https://github.com/SagerNet/sing-box/releases/tag/v1.15.0-alpha.13), [pinned TUN documentation](https://raw.githubusercontent.com/SagerNet/sing-box/v1.15.0-alpha.13/docs/configuration/inbound/tun.md), [migration](https://raw.githubusercontent.com/SagerNet/sing-box/v1.15.0-alpha.13/docs/migration.md).
- [Hysteria 2.13.0](https://github.com/HyNetworks/hysteria/releases/tag/app/v2.13.0) and [its pinned core dependencies](https://raw.githubusercontent.com/HyNetworks/hysteria/app/v2.13.0/core/go.mod).
- [TCP Brutal 2.0.1](https://github.com/HyNetworks/tcp-brutal/releases/tag/v2.0.1), [IPv6-capable rules](https://raw.githubusercontent.com/HyNetworks/tcp-brutal/v2.0.1/brutal_rules.c), [brutalctl](https://raw.githubusercontent.com/HyNetworks/tcp-brutal/v2.0.1/tools/brutalctl.c), [rule-application semantics](https://raw.githubusercontent.com/HyNetworks/tcp-brutal/v2.0.1/README.md).
- [Official Go releases](https://go.dev/dl/). Public Go module latest-version metadata was read from proxy.golang.org for the controller dependencies.
