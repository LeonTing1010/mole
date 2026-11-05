# Upload and response-header follow-up — 2026-11-05

## Outcome

The HY2 upload bottleneck is reproducible and is not explained by this short comparison as a sing-box-only limitation, by an individual proxy stream, or by the tested path-MTU setting. The controlled tests did not establish a production setting with at least 10% higher upload throughput and no regression. Production configuration, installed binaries, the node's queue and identity, DNS and PF were not changed.

The measurements below are short samples on one Wi-Fi connection and one node, not a long-term capacity estimate. Test clients used independent connections and were not the production HY2 standby instance. Every upload counted as successful only after the origin confirmed its length and SHA-256.

## Shared path and client controls

- In three rotating 2 MiB rounds the isolated REALITY+smux, REALITY without mux, HY2 with a 400 Mbps upload declaration, and production TUN medians were respectively 12.77, 8.81, 4.69 and 11.27 Mbps. Connection reuse, startup and the very small sample matter; the numbers do not isolate TUN overhead.
- Two 8 MiB comparisons of one versus four simultaneous streams gave HY2 4.52/4.51 Mbps versus 4.55/4.56 Mbps. The four streams share one configured HY2 outbound; this is consistent with a connection-wide or shared-path limit, but does not identify it.
- The official macOS arm64 Hysteria v2.13.0 release was checked against GitHub's published SHA-256 before use. Three alternating 4 MiB samples gave native Hysteria and sing-box medians of 4.64 and 4.62 Mbps. Both use the same node, UDP path, authentication and upload origin, but differ in implementation and potentially QUIC behavior. Similar results do not prove an ISP cause.
- With the same 400 Mbps HY2 upload declaration and initial packet size 1200, three alternating samples gave 4.76 Mbps with path MTU discovery disabled and 4.78 Mbps with it enabled. This does not establish the effective discovered MTU or cover other paths.

## Upload rate and QUIC signals

The first preliminary helper accidentally selected the 1 Mbps HY2 ladder rung. Its result is invalid for the 400 Mbps baseline and is retained separately as `upload-comparison-invalid-hy2-1mbps.json`. All comparisons below explicitly assert the 400 Mbps baseline.

In three rotating rounds of 4 MiB per sample, changing only the isolated sing-box HY2 upload declaration produced these medians:

| Upload declaration | Verified payload median |
| ---: | ---: |
| 8 Mbps | 4.78 Mbps |
| 16 Mbps | 4.71 Mbps |
| 32 Mbps | 4.75 Mbps |
| 400 Mbps | 4.54 Mbps |

The 8 Mbps result is about 5.2% higher than 400 Mbps, below the pre-existing 10% throughput threshold. An 8 Mbps ceiling could also prevent use of a faster path later.

Separate native-client debug runs, three paired rounds with a 4 MiB upload per run, reported far fewer QUIC packets classified as lost under the 8 Mbps declaration than under 400 Mbps. For example, one reported window at 8 Mbps had 2,588 acknowledged and 858 lost, versus 2,040 acknowledged and 62,193 lost at 400 Mbps. These are Hysteria's reported sender statistics; sampling windows can overlap and must not be summed as independent packets or equated to an independently measured physical-link loss rate.

During those six debug runs, 21/21 HTTPS probes per setting succeeded through the production REALITY helper. The observed median/p95 were 406/421 ms at 8 Mbps and 421/475 ms at 400 Mbps. These are short, clustered observations, not independent long-term p95 estimates. The debug harness waited for its last HTTPS probe after the upload completed, so its displayed throughput includes that wait and is not used for the throughput comparison above. Longer paired measurements across time, with precise upload boundaries, are needed before changing the backup configuration.

## Prior response-format anomaly

Raw response headers and complete bodies were checked against the loopback bulk endpoint using 16 concurrent clients. Production, isolated smux and isolated no-mux each passed 160 short 8 KiB responses; production and isolated smux each also passed 16 responses of 8 MiB. This did not reproduce the earlier sporadic leading zero and does not invalidate the recorded failure or establish a fix.

The inspected sing-mux implementation uses `0x00` as its successful stream-response status and consumes a status byte before passing response data to the caller. That makes a control-byte boundary a hypothesis worth investigating, not evidence of its source: no corresponding byte-level trace was captured at both sides of the failed historical request. The queue matrix was not resumed against the production interface.

## Resource and verification boundary

The upload origin was a task-owned loopback-only transient systemd service with an expiry. It was stopped, and its task-owned script was removed after tests. The official test binary and local test processes were removed. Private scripts, raw results, debug logs and cleanup receipt are under `~/.mole/diagnostics/throughput-followup-20261105/`; these may contain operational details and must not be committed. Previous node rollback backups remain untouched.

No source or production configuration change is claimed by this follow-up. A real sleep/lid cycle was not forced because the active protection policy destroys the node on lid closure. Further work should capture a reproducible byte-level failure and compare lower HY2 upload rates across multiple times and changing network conditions, including a faster uplink, before deciding whether an adaptive policy is warranted.

References: [Hysteria performance and receive windows](https://v2.hysteria.network/docs/advanced/Performance/), [Hysteria debug environment](https://v2.hysteria.network/docs/advanced/Environment-Variables/), [Hysteria v2.13.0 release](https://github.com/HyNetworks/hysteria/releases/tag/app%2Fv2.13.0).
