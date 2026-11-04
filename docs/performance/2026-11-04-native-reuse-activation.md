# Native reuse activation — 2026-11-04

The current supported node now uses REALITY/Vision + native smux, maximum eight
outer connections and minimum one stream before expansion. Actual production
reconnect generated that configuration and started new controller/engine PIDs.
The packet configuration changed only by addition of this multiplex option.
HY2 fallback, routing, DNS, MTU and the confirmed 600 Mbps REALITY target remain.

## Source behavior

- Fresh native REALITY identities opt into the multiplex capability installed
  by their boot template. The normal authenticated deployment verification
  must still pass. Legacy imports retain their explicit opt-in behavior.
- A bounded, line-framed live sensor observes only current-node REALITY TCP
  dial timeouts/refusals. It excludes origin errors, other nodes and other
  outbounds, handles ANSI/chunking and discards oversized records.
- Recent node-dial failures bring clean-window sampling back from the healthy
  one-minute interval to the normal 15-second keepalive cadence. Passive
  sightings never become failed probe votes or directly move the selector.
  Local-link suspension and inactive/fallback-leg guards remain.
- Status/state expose raw application node-dial sightings separately from the
  synthetic probe window. They are counts since this daemon start, not rates.

The change follows RED/GREEN constraints. Tests cover both fresh deployment
protocol shapes and legacy imports, framed/filtered/IPv6 failure evidence,
oversized log recovery, sampling rate limits, stale evidence, bad-link and
fallback/pin guards, and separation of displayed observations from probe votes.

## Build and installation provenance

Implementation commit: `a6828fc8efdc5e408bac9a0f7bdc2926462f549b`.
Controller compiler: Go 1.26.10. Installed packet engine remains sing-box 1.14.4.
The project verification script passed all Go race tests, six Python tests and
the production build; `go vet ./...` and diff checks also passed.

The pre-existing installed controller already contained an unrelated local
uncommitted DELETE-result verification fix. The installation build preserved
that exact source/test overlay privately, reran the full gates and vet with it,
and installed the resulting candidate. The original working-tree files were
unchanged and the overlay was not committed as this task's work. The private
receipt records its hash, candidate hash, previous executable and rollback state.

## Production acceptance

- Twelve fresh OpenAI API/ChatGPT TLS requests received their expected 401/403
  responses; two independent external HTTPS/body controls also passed: 14/14.
- Eight application flows through the production TUN delivered exactly 64 MiB,
  all requests complete, about 138.8 Mbps in this short observation.
- API TLS-ready median was about 318 ms; ChatGPT about 119 ms. These are local
  post-activation samples, not a matched before/after percentage or model TTFT.
- Confirmed 600 Mbps pacing, armed lid-destroy policy, enabled PF and 80 Mole
  anchor rules were read back. Node exemptions were verified in PF's actual
  optimizer-created address tables, not merely in the generated rules file.
- The installed candidate hash and preservation of the prior local fix were
  checked after activation. No new production ENOBUFS was observed.

The first acceptance script incorrectly required the node's literal address in
the displayed PF rules. PF had folded address sets into automatic tables. The
script triggered a successful rollback, retaining the failed receipt. After
independent PF inspection, acceptance was corrected to check actual table
membership plus enabled PF/block rules. The subsequent activation passed; the
first failure was not suppressed or converted into a success.

OpenAI probes used no credential or generation request. API 401 and ChatGPT 403
confirm transport/HTTP responses, not authenticated model or stream acceptance.
Sustained application performance, sleep/wake and model first-token latency
remain separate acceptance work. The measured native/short-test improvement is
not a long-term throughput or availability guarantee.
