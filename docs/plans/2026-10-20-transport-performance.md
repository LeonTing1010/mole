# Plan: reduce connection setup stalls without sacrificing bulk throughput

## Context

The running client uses non-multiplexed VLESS/REALITY Vision on TCP/443. On 2026-10-20 its log recorded 125 I/O timeouts over roughly five hours, including failed TCP dials to the node, with zero ENOBUFS, fallback transitions, or local-link latches. Eight-connection short downloads reached 244 Mbps on TCP, 197 Mbps on HY2, and 202 Mbps on both. More parallel connections alone do not establish a transport improvement.

## Goal

Use the protocols' existing transport capabilities to reduce fresh-flow setup cost while preserving authenticated transport, legacy Vision clients, node pacing, and existing routing controls. Promote only changes with same-node, same-endpoint measurements; preserve negative results.

## Verification conditions

- Existing Vision credentials and HY2 keep working.
- A candidate must prove keeper body delivery, fresh external HTTPS delivery, and bounded bulk throughput.
- Compare equal connection counts on the same endpoint; distinguish startup-inclusive and sustained throughput, and report failures separately.
- Regression constraints fail before implementation and pass afterward.
- `bash scripts/verify-performance.sh /tmp/mole-performance-20261020` passes.
- Runtime changes have a rollback; unprivileged checks are not reported as a running daemon upgrade.

## Approach and tasks

1. Isolate a native TCP multiplex experiment on the owned node, restricted to this client's address, with automatic cleanup. Compare Vision and non-Vision smux on the same listener before selecting an implementation.
2. If the experiment qualifies, add the smallest optional client/server configuration contract that preserves Vision credentials and avoids incompatible Vision/mux combinations. Cover serialization and actual sing-box behavior with regression constraints first. If it fails, retain the negative result and investigate the measured bottleneck instead.
3. Run repository gates, build a reviewable artifact, activate only verified changes that the available permissions allow, and repeat the same measurements. Keep code verification separate from runtime performance evidence.

## Risks

TCP multiplexing shares loss across streams and can reduce bulk throughput. Different ports and transient path conditions can confound results. The local daemon runs as root and passwordless sudo is unavailable; any upgrade needing local elevation must remain explicitly pending. No provider/node replacement is included.
