# Plan: deployment recovery safety

## Context and goal
The latest deployment fallback can outlive cancellation, race first boot, and lose recovery access. Repair all seven review findings without modifying live nodes.

## Intent and verification
1. Cancellation stops repair work and SSH, while firewall cleanup remains bounded and independent. Verify canceled repair performs no work and a running SSH child terminates.
2. Boot and repair share a node-side lock across identity inspection and mutation. Verify a held boot lock prevents repair writes; preserve existing identities and update local identity only when replacement actually occurred.
3. Install SSH recovery before bounded prerequisites. Verify a stuck prerequisite ends within its budget and recovery keys already exist.
4. Package lock detection works without fuser and all recovery commands share a deadline and account lock. Verify contention with POSIX locks and an expired configuration budget.
5. Repair the requested failed legs, including HY2 binary/config/certificate restoration, preserving the healthy leg. Verify generated repair behavior in a local shell sandbox.
6. Low RTT is advisory and cannot authorize instance deletion. Verify near-datacenter responses never produce a destructive verdict.
7. Persist repair verification errors rather than declaring unrecorded success. Verify persistence errors propagate.

## Workflow and acceptance
Write failing behavioral constraints first, implement sequentially, then run `scripts/verify-performance.sh`, go vet, Linux cross-build, shell syntax, generated-asset consistency and diff checks. No cloud operations or daemon installation. Keep source on a persistent Git branch; remove owned temporary resources after integration if authorized.

## Completed local verification
- Nine new regression tests cover prerequisite deadlines and recovery keys, expired package recovery, cancellation, SSH child termination, failed-leg restoration, POSIX lock contention without fuser, boot/repair exclusion for both protocols, advisory RTT, and persistence failures.
- Baseline failures were observed for recovery-key ordering, expired package configuration, low-RTT deletion, and missing cancellation interfaces. A held POSIX lock advanced immediately on the old fallback and blocked correctly after the fix.
- `bash scripts/verify-performance.sh /tmp/mole-deploy-recovery-verified-20261112`: Go race suite and all 11 Python tests passed; native binary built.
- `go vet ./...`, Linux amd64 cross-build, shell syntax checks and `git diff --check` passed.
- No cloud requests, instance changes, daemon replacement, or live VPS acceptance were performed. Cleanup of temporary build outputs is independent of source verification.
