# Plan: Retire automatic mixture optimization

## Goal
Use REALITY as the automatic primary and Hysteria2 as the confirmed-fault
fallback. Retain explicit protocol pins, local-link safeguards, recovery/dwell,
TCP pacing, scheduled ceilings, the opt-in rate governor and existing ENOBUFS
self-healing. Remove the separate mixture controller and its periodic downloads.

## Intent and verification

1. Retire the public `both` mode safely. RED: a persisted legacy both pin reads
   as auto; writing/requesting both fails without changing an existing valid
   pin. Dual-protocol configurations have one traffic selector, defaulting to
   REALITY, with no source-port shards. Existing explicit routes stay supported.
2. Remove allocation sampling, flow selectors, mixture status and duplicate
   health/UDP-pressure machinery. Keep the existing auto failover, manual pins,
   probe forwarders and self-healing tests. Preserve a real-engine test showing
   that changing the one selector affects new connections while established
   streams keep their original connection.
3. Update CLI/documentation to the three supported modes, verify complete race,
   Python and build gates, then commit, merge to master, remove the task branch
   and temporary worktree. Keep evidence under
   ~/.mole/diagnostics/primary-fallback-20261115.

## Limits
Source changes do not update the installed program or restart the live tunnel.
No new throughput claim or live route change is needed for this removal.
