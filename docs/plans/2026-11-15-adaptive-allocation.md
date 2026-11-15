# Plan: Treat single-leg routing as an allocation endpoint

## Goal
In `mole leg both`, choose new-flow allocations from 8:0 through 0:8 using
matched measurements rather than a fixed 6:2 split. Keep `auto` fault-only
semantics and explicit pins compatible. Do not activate or install this change.

## Intent and verification
1. Express healthy allocation as a measured preference, initially 8:0.
   RED/GREEN: every allocation including endpoints is eligible; require two
   consecutive complete comparisons with >15% improvement; reject invalid or
   partial results; health withdrawal overrides performance immediately.
2. Measure allocation mixtures with eight total concurrent streams, one shared
   elapsed window and validated complete payloads. RED/GREEN: all nine mixtures
   preserve the same connection/payload budget; any bad stream rejects a sample.
3. Integrate bounded scans and publish evidence. RED/GREEN: only qualified,
   stable, pressure-free `both` mode may sample; abort/discard when mode, link,
   rate, rung, or overflow changes. A scan is at most 72 MiB every ten minutes;
   each mixture has a five-second timeout. No selector changes during sampling.

## Final verification
Run scripts/verify-performance.sh (race tests, Python tests, build), inspect diff
and secrets, commit on a durable task branch, remove temporary worktree.

## Risks
Synthetic keeper downloads do not prove application or upload performance.
Sequential measurements can drift; alternate scan order and require repeated
wins. Periodic probing consumes bandwidth only while `both` is selected.
