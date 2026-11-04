# OpenAI connection speed and stability audit — 2026-11-04

## Result

There is measured scope to reduce connection setup and some reconnection delays.
Native REALITY/Vision + smux is the strongest first candidate on the current
node. Preserve application HTTP/2/WebSocket connection reuse and the qualified
HY2 fallback. Neither a higher Mbps declaration nor a fast HTTP rejection proves
faster model generation.

Production settings and selectors were preserved. This audit does not activate
smux, restart the daemon, change DNS/routing, deploy a node or invoke a model.
Private receipts retain raw failures and test tools outside Git. The report and
verification plan are the only tracked changes.

## Measurement boundaries

The active local packet engine was official sing-box 1.14.4. Tests copied the
current node's authenticated REALITY or pinned/obfuscated HY2 outbound into
temporary unprivileged engines. Each used its own loopback listener and en0
binding; no TUN, system DNS, selector, pacing target or certificate bypass was
introduced. Credential-bearing temporary configurations were mode 0600 in
private directories and removed with their owned processes.

OpenAI API probes used unauthenticated `GET /v1/models` and received 401.
ChatGPT probes used `HEAD /` and received 403. These are verified TLS plus HTTP
response observations, **not** authenticated API acceptance, logged-in chat,
WebSocket/SSE delivery, model first-token time or token generation speed.
Response headers identified Cloudflare and an ICN Ray suffix; this does not
locate OpenAI's model computation. See the primary
[Cloudflare header documentation](https://developers.cloudflare.com/fundamentals/reference/http-headers/).

The bulk endpoint belongs to the existing node's loopback keeper. It measures
node-to-Mac payload through the encrypted transport, not OpenAI transfer
capacity, local upload speed, all websites or sustainable ISP capacity.

## Where time and failures occur

| Segment | Current evidence | Implication |
|---|---|---|
| Wi-Fi / first hop | Live 5 GHz, 80 MHz, 802.11ax; recent monitor window 12/12 gateway echoes, median about 3 ms | Keep the better wireless attachment; no evidence here requires new hardware |
| Local DNS | 18 local A/AAAA/HTTPS queries; median about 0.10 ms; foreign A uses FakeIP | A public DNS replacement is not a demonstrated local speed fix; node-side real DNS is separate |
| Physical Mac-to-node TCP | 30/30 interface-bound connects; median 108 ms, p95 306 ms, max 867 ms | A meaningful setup floor and variable tail remain even with a healthy local hop |
| Proxy session setup | No multiplex in production; isolated smux retains an observed idle outer connection | Reusing the outer session can avoid repeated node TCP/REALITY setup |
| Node-to-OpenAI | No readback: SSH over the authenticated node tunnel rejected the configured local key | Node CPU, kernel, running server version, real DNS timing and IPv6 treatment remain unobserved |
| Application connection reuse | Same curl HTTP/2 process reports `num_connects=1,0,0,0,0,0`; active real ChatGPT flows also persisted for minutes | Reuse is possible and actual application flows can persist; the HTTP/SSE protocol of those encrypted real flows is not directly observed |
| Model processing | No authenticated generation request was made | Network improvements cannot be converted into a model speed multiplier |

Local curl's `time_connect` is the connect to a FakeIP/TUN or loopback SOCKS
listener. It must not be interpreted as the physical node RTT. The physical
connect sample above explicitly bound the socket to the uplink. `time_appconnect`
is cumulative elapsed time until TLS is ready, not a pure TLS CPU measurement.
The [curl manual](https://curl.se/docs/manpage.html#--write-out) defines these
timers and the per-transfer connection count.

The state's Wi-Fi PHY string was historical: source updates it on a bad-link
episode, not every association change (`core/linkprobe.go:203`). Live
`system_profiler` confirmed 5 GHz while that state string still said 2.4 GHz.
Do not use the stale string to classify a new measurement.

## Real failure evidence and the monitoring gap

The current daemon log contained 29 ChatGPT errors explicitly dialing the
node's TCP/443 socket and 8 additional ChatGPT connection deadlines. These
counts accumulated over the current daemon lifetime; total application attempts
were not available, so they are **not** error rates. Estimated wall times from
engine uptime placed five explicit node TCP errors and one unspecified deadline
after 12:33, following the 5 GHz switch. The clock mapping has startup-offset
uncertainty; it is not a packet capture.

The explicit `dial tcp <node>:443: i/o timeout` errors occur before OpenAI
application processing. This locates a failure boundary; it does not identify
an ISP shaper, Wi-Fi fault, firewall, TFO issue or node resource failure as its
cause. A separate application dominated the overall error count; that is not
evidence it caused the ChatGPT failures or consumed the available capacity.

The fresh monitor can have a clean 20-sample window while application flows
fail. Source stretches clean fresh sampling to once per minute
(`core/supervisor.go:778`, `:881`). Its two-origin race deliberately accepts an
independent successful origin (`core/egress_targets.go:35`); one origin's failure
must not condemn a working transport. Failover requires failed samples and a
paired, spaced candidate qualification (`core/supervisor.go:1000`, `:1069`).

A useful future change is to let passive **node-dial** failures request earlier
comparative sampling, while keeping local-link gating, generation checks and
the existing qualified handoff. Raw log errors should not directly flip the
selector or cause a per-domain routing proliferation. That behavior has not
been implemented or qualified in this audit.

## Same-node native protocol experiment

Three rotated rounds used identical origins and three fresh requests per origin
per case per round: nine samples per host, 18 per case, 72 comparison samples.
New curl processes created fresh inner TLS. Smux/HY2 could reuse their warm outer
session; that intended mechanism is part of the comparison. Both credentials
and certificate verification were preserved.

| Configuration | API TLS ready, median | API first HTTP byte, median | ChatGPT TLS ready, median | ChatGPT first HTTP byte, median |
|---|---:|---:|---:|---:|
| Current Vision shape | 480 ms | 1092 ms | 368 ms | 489 ms |
| Vision + native smux 8/1 | 123 ms | 545 ms | 124 ms | 281 ms |
| Vision, TFO disabled | 348 ms | 775 ms | 347 ms | 461 ms |
| HY2 / 400 Mbps | 323 ms | 691 ms | 116 ms | 231 ms |

All 72 comparison attempts produced verified TLS/HTTP responses. Including
pilots, application-reuse sequences and bursts, the first experiment retained
152/152 transport responses. Small successful samples do not establish a
long-term failure rate or a reliable population p95.

For the API, smux cut observed median TLS-ready elapsed time about 74% and the
median unauthenticated HTTP first-byte time about 50%. The API's nine-sample
upper tail moved much less: its empirical p95/max was 1.31 s versus 1.51 s.
This is evidence of a useful setup improvement, not complete tail elimination.

Smux did not win every situation. In one four-request burst, mixed-origin
first-byte median was 1.12 s versus Vision's 0.71 s. The origins and trial order
were the same across cases, but only one burst per case was recorded. This is
a counterexample to a universal speedup, not a statistical concurrency verdict.
Disabling TFO likewise had no consistent advantage across setup, origins and
burst behavior, and no failure difference was established. Keep it unchanged.

The [native multiplex contract](https://sing-box.sagernet.org/configuration/shared/multiplex/)
documents the connection and stream limits. Current `config/parser.go:609`
supports explicit `mux=smux` with max connections 8 and min streams 1. This
requires actual server support, which authenticated delivery confirmed for this
node. Idle socket inspection observed one retained smux outer TCP connection.
Shared TCP loss can still affect multiple streams; an eight-connection ceiling
does not prove independent transport for every application flow.

### Cold startup and application reuse

The first pilot had 2.6–3.8 second first requests across all four candidates.
Those outliers were retained. A separate three-round rotated fresh-engine test
did not reproduce that same multi-second pattern and did not identify its cause.
Its API TLS-ready medians were:

| Configuration | First request after engine start | Next fresh application request on same engine |
|---|---:|---:|
| Vision | 614 ms | 354 ms |
| Smux | 362 ms | 115 ms |
| HY2 | 566 ms | 117 ms |

Smux/HY2 do not eliminate the first cold outer handshake. Compare warm and cold
cases separately; dropping pilot outliers would conceal an unresolved boundary.

On the actual production TUN, six sequential curl HTTP/2 ChatGPT edge responses
used connection counts `1,0,0,0,0,0`. First HTTP byte fell from 456 ms initially
to about 111–112 ms on the final four reused transfers. For API 401 responses,
reused response times still varied from 263 to 497 ms. These do not measure
ChatGPT generation or prove the real application uses exactly that curl path.

The first attempted HTTP/1.1 idle test was invalid for persistence: ChatGPT
explicitly closed its 403 response; the client could reconnect afterward. Its
subsequent 400 rows are not retained-session success. A separate curl scheduling
attempt used an unsupported interval spelling and executed zero transfers.
Both negative experiments are retained and excluded from idle acceptance.

The corrected HTTP/2 test used four API requests per case, with approximately
30 seconds between transfer starts. Vision, smux, HY2 and the actual production
TUN each returned four verified TLS/401 responses over about 90 seconds. Every
case reported `num_connects=1,0,0,0`; subsequent TLS setup was zero because the
same application connection remained open. Production's three reused response
times were 303, 261 and 563 ms. Smux's first response was a retained 2.27-second
outlier, followed by 448, 277 and 284 ms on that same HTTP/2 connection.
This qualifies the observed 30-second idle intervals; it does not qualify
multi-minute silence, sleep/wake, path replacement or authenticated streams.

## Bulk and concurrent-response checks

Each main bulk condition had equal 64 MiB counted warmup, 32 MiB single-flow and
64 MiB eight-flow measurements. The eight-flow phase repeated in reversed case
order. All complete phases received their exact budgets.

| Configuration | Single-flow | Eight-flow, forward / reverse |
|---|---:|---:|
| Vision | 52.2 Mbps | 136.0 / 142.9 Mbps |
| Smux 8/1 | 60.8 Mbps | 204.6 / 202.7 Mbps |
| HY2 | 90.8 Mbps | 150.7 / 162.1 Mbps |

These isolated helpers bypass TUN packet processing. They support selecting a
candidate; they do not promise a production-TUN rate. REALITY kept its existing
600 Mbps target and HY2 its existing 400 Mbps declaration. They therefore are
not an equal-rate congestion-controller causal experiment.

A separate load test used eight 16 MiB flows per case plus fresh API/ChatGPT
HTTPS checks:

- Smux delivered all 128 MiB; concurrent HTTP responses 8/8, median 492 ms,
  maximum 1.91 s.
- Vision delivered 112 MiB: one of eight bulk requests was reset before body
  delivery. Its incomplete phase must not be ranked using its apparent Mbps.
  Concurrent HTTPS responses were 5/5, median 971 ms, maximum 1.22 s.
- HY2 delivered all 128 MiB; concurrent HTTP responses 5/5, median 1.26 s,
  maximum 5.55 s. This tail argues against selecting it as an unconditional
  replacement for every workflow.

Different completion durations yielded different concurrent-check counts. One
reset and a few tail samples do not establish comparative long-term failure
probabilities. No experiment observed new production ENOBUFS.

## Recommended order and remaining acceptance

1. Qualify a production-TUN canary enabling native smux on the current supported
   node. Preserve the current node-specific profile for rollback; compare fresh
   setup, application errors, long-lived chat/stream continuity, sleep/wake and
   foreground responsiveness before and after. This audit did not perform that
   activation or claim authenticated application acceptance.
2. Preserve long-lived application HTTP/2/WebSocket sessions. OpenAI's official
   [WebSocket mode documentation](https://developers.openai.com/api/docs/guides/websocket-mode)
   explains persistent connections for repeated tool turns; this is application
   functionality, not a switch Mole can impose on an encrypted client. Current
   authenticated WebSocket use and first-token measurements remain unobserved.
3. Preserve HY2 as the qualified fallback and consider passive node-dial errors
   as a reason to sample sooner. Keep measured handoff gates instead of making
   every slow request or origin rejection force a route change.
4. Carry the chosen native-reuse preference across fresh node creation only
   after server capability verification. `cmd/server.go:129` defaults absent
   multiplex state to false; `configureDeployCeiling` recalls bandwidth but not
   multiplex. New boot templates enable server support, yet a newly created
   client profile does not automatically opt in. This explains why a prior
   node's tested option is absent today; it is not evidence the new server
   cannot support it.
5. Leave DNS, MTU, global congestion control, node tier and IPv6 policy alone
   until a matched experiment identifies their contribution. Node SSH rejection
   prevented current server readback. A source comment saying only one DNS
   address is attempted is insufficient for a current-version conclusion:
   [1.14.4's resolver](https://raw.githubusercontent.com/SagerNet/sing-box/v1.14.4/common/dialer/resolve.go)
   has explicit parallel and serial address-list paths. Actual remote engine
   version and path selection still require verification.

OpenAI's [latency guide](https://developers.openai.com/api/docs/guides/latency-optimization)
also separates request/stream behavior from token processing. A narrower local
network setup time does not accelerate remote model computation by the same
percentage. Authenticated first-token latency, inter-token gaps, disconnects and
resumed streams should be the final user-facing acceptance metrics.

## Reproducibility and preservation

Private receipts: `experiment.json`, `path-inventory.json`, `stability.json`,
`cold-and-idle.json`, invalid-test receipts, edge-header receipt and private
experiment scripts. No API key or model request was used. SSH attempted only
the existing configured local identity through the authenticated tunnel, used
temporary known-host storage, and made no remote change after key rejection.
No remote ingress, module, service or firewall was changed.

The public report omits credentials, node identifiers, raw logs and local
account data. Final acceptance validates exact successful byte counts, retains
failed budgets, validates delayed HTTP/2 connection counts, confirms owned
engine cleanup and compares production configuration/server-store hashes.
The main protocol experiment delivered exactly 672 MiB. Separate load-test
warmups delivered 192 MiB; the load phases delivered 368 of 384 MiB, retaining
the failed Vision request. The combined owned-node payload was 1232 MiB.
No repository source change required the Go build/race-test gate in this audit;
documentation checks and receipt invariants apply to these artifacts.
