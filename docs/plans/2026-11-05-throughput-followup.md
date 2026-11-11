# Plan: investigate remaining throughput limits

## Goal and acceptance

Identify reproducible upload or transport bottlenecks and apply only changes that improve a matched baseline without new errors or regressions. Preserve the active connection, DNS, PF protections, node identity, and a tested rollback path. Report missing evidence rather than infer a cause from throughput alone.

## Work

1. Refresh runtime and prior experiment evidence; map upload and malformed-response paths through the installed engine and tools. Verify the transport and server-received length/hash for each valid sample.
2. Isolate the upload bottleneck with bounded, matched runs and capture relevant TCP/QUIC, process and network counters. Change one variable at a time. Compare both throughput and failures under the same workload.
3. Investigate the malformed HTTP response at the byte and protocol boundaries. Introduce a regression test only if a specific code defect can be demonstrated.
4. Resume queue and multiplex comparisons only if the measuring path is sufficiently reliable. Keep the current settings if repeated comparisons do not clear the acceptance threshold.
5. For any justified change, implement in the isolated branch, run the applicable behavior tests, Go race suite, Python suite, build and vet. Activate only with rollback and independent protocol, DNS, protection and end-to-end checks.

## Guardrails

- No forced changes to the shared working tree, unrelated files, service identity or persistent node configuration.
- Never treat a failed, incomplete or mismatched-hash transfer as a successful throughput sample.
- Retain baseline, failures, exact settings and rollback evidence; delete only task-owned temporary resources after checking active use.
