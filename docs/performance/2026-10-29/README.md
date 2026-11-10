# 2026-10-29 reliability changes and current Wi-Fi checks

Code commit: `d0f3f4f36ad902d5c4ff47437eca1b43eb67eac2`.
Existing smux/equal-concurrency work was preserved separately in `6f485e4`.

The fixed commit passed `bash scripts/verify-performance.sh /tmp/mole-network-reliability-20261029`:
Go race checks for every package, six Python tests, and a macOS external-link build.
Tests cover error-interrupted qualification, retry/backoff, malformed reload preserving
last-good files, and deadline/short-body measurement failures. The installed executable
matches that artifact by SHA-256; see `install-receipt.json`.

Probe output separates slow successes and failures and records the last sample time.
Both mode describes its fixed health-gated source-port buckets and actual probe errors.
Rule recompilation retries unchanged failures (2–60 seconds), rejects malformed custom
JSON, and states the DNS reconnect requirement. Per-file rule-set replacement is not
a multi-file transaction. Existing failover thresholds and simple auto are preserved.

`native-ab.py` is the exact isolated same-node experiment except that its repository
path is relative. It creates no TUN and does not modify production config. Private
configs are mode 0600 under a temporary directory, then removed after owned clients exit.
Run with `PYTHONDONTWRITEBYTECODE=1 python3 docs/performance/2026-10-29/native-ab.py`.
It downloads at most 320 MiB in total and writes `/tmp/mole-native-ab-20261029.json`.
`native-ab.json` preserves both rounds, including slower observations.

| Total concurrent downloads | Plain round 1 / 2 Mbps | smux round 1 / 2 Mbps |
|---|---|---|
| 1 | 29.31 / 25.29 | 29.20 / 33.04 |
| 8 | 122.61 / 122.84 | 79.18 / 123.24 |

All eight payload phases delivered their budgets, and all twelve small HTTPS checks
succeeded, including client restart checks. These are short transfers, not sustained
capacity certification. Different rounds reverse protocol order; budgets are equal
within each concurrency group (16 MiB single, 64 MiB eight flows). Cold/warm is recorded
for HTTPS only, not a separately randomized cold/warm capacity experiment. Upload,
server/network failure recovery, wired performance, concurrent competing traffic and
cross-time stability remain unvalidated. Local build/check activity and background
traffic were not fully quiesced. **No consistent throughput gain was established;
production smux remains disabled and auto remains selected.**

`wifi-legs.json` is a separate short 32 MiB-per-phase, eight-total-connection run
through the existing daemon forwarders: Reality 31.3, HY2 124.1, Both 75.7 Mbps.
Every phase completed with no request error or new ENOBUFS. These sequential short
observations differ from the isolated client results; they do not identify the cause
or justify a permanent protocol switch. They are not a 500 Mbps ISP test.

The CLI is installed with a verified backup. At the recorded install time the old
root daemon remained active because `sudo -n true` required a password. A local user
must run `mole down && mole up` (without putting sudo before up) to activate daemon
changes and preserve normal key handling. Then verify PID/config, shield, auto,
lid-close protection, probe freshness and real requests. Rollback: replace
`/usr/local/bin/mole` with the saved `mole.before`, then reconnect the same way.
