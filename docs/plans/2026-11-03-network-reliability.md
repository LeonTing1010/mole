# Plan: qualify recovery and verify performance

## Goal

Apply the six audited corrections without losing the current dirty workspace or
changing transport defaults from an unverified speed claim. Compare recovery
behavior against the audited baseline, then measure bounded throughput under
identical conditions. Production activation requires local administrator
authentication; prepare and verify everything possible before that step.

## Verification conditions and tasks

1. Recovery: first reproduce the old keepalive route/state split. Require three
   authenticated candidate checks and a successful selector commit before
   clearing evidence. Preserve fallback and evidence on failed writes, bad
   candidate responses, operator pins and bad local links.
2. Monitoring: independently validate two external HTTPS targets under one
   deadline. One valid target prevents origin-specific false failover; both
   failing still count. Keep warm and fresh HTTP connection policies separate.
3. Restart: generate a private, stable cache file with FakeIP persistence.
   Verify real installed-engine body delivery before and after restart without
   refreshing the application's DNS result.
4. State: reproduce concurrent temporary-file collisions; require independent
   state writers to finish without errors and preserve parseable JSON.
5. Capability: unconfirmed TCP pacing remains visible and retried but cannot
   suppress authenticated protocol recovery. Do not overwrite a newly applied
   fallback with the scheduler's earlier target.
6. Packet settings: remove the unsupported server MTU key and its client-side
   inference. Preserve conservative packet defaults until measured A/B evidence
   warrants a change. Validate full TUN and PMTUD candidates separately.

Each business change follows RED then GREEN with a reason in the constraint.
Final gates: Go race tests, Python tests, vet, diff checks, build and installed
sing-box config checks. Only named deltas are integrated. Preserve rollback
binary, source/config hashes and raw test results. No billable node is created.

## Performance boundary

Owned-node forwarders measure transport payload and bypass TUN. Full TUN stack
comparisons require an administrator-controlled run. Never describe a forwarder
result as a TUN benchmark or a short sample as sustained Internet capacity.
