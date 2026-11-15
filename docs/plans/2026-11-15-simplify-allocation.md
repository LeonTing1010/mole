# Plan: Simplify adaptive allocation

## Goal
Keep the existing measurement guards, budgets and confirmation thresholds while
reducing duplicated routing rules, derived state and mixed lifecycle operations.
Keep the live daemon unchanged during source work.

## Tasks

1. Give source-port lanes one mapping shared by selector writes, reconciliation,
   keeper endpoints and flow checks. Remove the unused legacy allocation-download
   wrapper. Derive desired lane count from health and confirmed preference rather
   than storing a second copy. Verify all shares, endpoints, manual rollback,
   healthy/failed transitions and UDP pressure behavior with existing tests;
   extend the selector/keeper cross-check to every allocation.
2. Separate finishing a trial from starting one. Write a failing behavioral
   regression for a trial finishing after selector reconciliation: it must wait
   until the next reconciliation, so the reported decision and applied routes
   cannot disagree. Use guard clauses for result validation and centralize
   cancellation plus pending-evidence reset. Verify cancellation, cooldown,
   failure handling and the new timing boundary under the race detector.
3. Remove unused observation fields and redundant confirmation arguments; keep
   search state and its operations together, using standard sorting for the
   median. Preserve the measured-noise, drift and three-confirmation tests.

Each task is a separate reviewable commit. Run focused tests for each; finish
with the project's full performance verification script, diff/security review,
source-tree readback and cleanup of the isolated task worktree. Preserve logs in
~/.mole/diagnostics/allocation-simplify-20261115. Merge the verified task branch
into master and remove it under the user's existing integration instruction.

## Limits
This refactor establishes source behavior, not a live throughput improvement.
No network-capacity probes, installation or daemon restart are needed.
