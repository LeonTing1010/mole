# Native host capability measurements — 2026-11-03

No tested candidate established a repeatable download gain. Keep the current
IPv4 REALITY/smux path, gVisor MTU 9000, 400 Mbps declared rate, existing socket
limits and original battery low-power setting. No product code or installed
binary was changed, and the production VPN was not restarted.

## Host and observed controls

The live host reports Apple M5, 10 CPU cores, 16 GiB RAM and macOS 27.2.
The active Wi-Fi link was 802.11ax on channel 149, 5 GHz / 80 MHz, around
-59 dBm RSSI and -95 dBm noise. One observed PHY rate was 544 Mbps; this is not
application delivery capacity. There was no active Ethernet interface.
The Mac was on battery with low-power mode enabled.

TCP Fast Open (`3`), TCP segmentation offload (`1`), receive auto-tuning and
previous Mole UDP buffer tuning were already enabled. Captured original values:

- `kern.ipc.maxsockbuf=8388608`
- `net.inet.tcp.autorcvbufmax=4194304`
- `net.inet.tcp.autosndbufmax=4194304`
- battery low-power mode `1`, AC low-power mode `0`

## Controlled full-TUN comparisons

Independent root-authenticated sing-box 1.14.3 clients used the same node and
REALITY credentials, TLS settings and smux 8/1. A private DNS listener created
native FakeIP mappings in an owned range. Only the two corresponding host routes
were temporarily directed to utun30. Application sockets were unbound, unlike
the earlier interface-bound harness. No global default route or system DNS
setting was changed. The clients downloaded counted payload from the node's
loopback keeper, and validated external HTTPS before each measured session.

Each control comparison alternated order over two rounds, used 16 MiB with
one flow and 64 MiB with eight flows, and started identical fresh engines.
These are primarily short/cold-session comparisons. All 26 measured full-TUN
phases delivered their complete budgets, with no new production ENOBUFS.

| Eight concurrent flows, median | Observed baseline | Candidate |
| --- | ---: | ---: |
| TUN MTU 9000 versus 65535 | 211.4 Mbps | 205.0 Mbps |
| Battery low-power mode on versus off | 213.4 Mbps | 212.0 Mbps |
| Socket ceiling 8 to 16 MiB and TCP auto limits 4 to 8 MiB | 215.5 Mbps | 215.1 Mbps |

The single-flow results also did not show a consistent material gain. Engine
CPU time during eight-flow tests was around 25–30% of one core, not a full-core
load. That observation does not identify the remaining path bottleneck.

mixed failed the owned-node pilot, and remained unsuccessful in a follow-up
with an explicit route for its NAT peer address. Do not promote it or infer its
ordinary production automatic-route behavior from these isolated tests.

Every OS profile was restored after each case and again at suite exit. The
observed before/after controls match, all owned interfaces were removed, and
the production configuration hash remained unchanged. See `tun.json`,
`power.json`, `buffer.json` and `peerpilot.json` for the complete data.

## Native IPv4/IPv6 and destination controls

Both address families authenticated and passed all 22 external HTTPS checks.
Three alternating-order native-client rounds used equal 1/8 concurrency and
complete budgets. IPv6 was slower than IPv4 in every paired throughput round.
Native eight-flow medians were 351.0 Mbps for IPv4 and 148.0 Mbps for IPv6.
Keep IPv4; these numbers bypass TUN and are not production TUN speed results.

The current keeper implementation in `cmd/brutal.go` matches IPv4 peers and
installs `/32` pacing rules. Therefore this is a comparison of the current full
configurations, not an isolated claim about IPv6 routing quality or its future
performance with equivalent pacing. No remote configuration was changed.

A separate two-round literal-IP / localhost control passed 18 external HTTPS
checks and delivered all eight throughput budgets. Within matching rounds,
using localhost instead of 127.0.0.1 produced essentially the same eight-flow
rate. Later reused/warmed native sessions were much faster than their early
samples. Those reused-client results cannot be compared with fresh full-TUN
sessions to calculate a TUN overhead percentage or justify bypassing TUN.

All 46 throughput phases across the experiments delivered their complete
budgets (1840 MiB total). These short Wi-Fi downloads do not establish sustained
Internet capacity, upload speed or long-term availability. No public speed-test
origin, wired link, router reconfiguration or engine upgrade was qualified.

## Final verification and reproduction

`receipt.json` contains medians, limitations, configuration/binary hashes and
restoration assertions. Final live TLS checks are in `postcheck.json` and live
status in `status-after.txt`. The previously verified binary and source manifest
remain unchanged. The same daemon continued running; kill switch, lid policy,
400 Mbps pacing and private FakeIP cache remained active. No reconnect is needed.

The recorded scripts read credentials from the existing private Mole config;
none are embedded in the artifacts. `measure.py tun`, `measure.py power`,
`measure.py buffer` and `measure.py peerpilot` require administrator rights,
run bounded experiments and restore their own changes. Native-client scripts
`address-ab.py` and `destination-control.py` do not change the system profile.
They assume this host's interface naming and the matching owned-node keeper;
review them before reuse on another machine.

The experiment plan is `plan.md`. No new VPS was created and no unrelated dirty
work was staged, committed or restored.

## Primary implementation references

- [Pinned sing-box 1.14.3 TUN implementation](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.3/protocol/tun/inbound.go): CLI MTU defaults and Darwin packet handling. GSO configuration has been removed; it was not enabled as a speculative optimization.
- [Pinned sing-tun Darwin implementation](https://raw.githubusercontent.com/SagerNet/sing-tun/bca4bf029237/tun_darwin.go) and [system stack](https://raw.githubusercontent.com/SagerNet/sing-tun/bca4bf029237/stack_system.go): native batching and NAT-peer mechanics used to inspect the test harness.
- [Apple XNU receive auto-tuning](https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/netinet/tcp_input.c) and [TCP setup](https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/netinet/tcp_subr.c): implementation context. This public source branch is not asserted to be the exact running kernel; live sysctl readbacks are authoritative for tested control values.
