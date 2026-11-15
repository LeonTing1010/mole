# Plan: Make adaptive measurements safe and reproducible

## Goal
Replace one-shot maximum selection with bounded paired trials that tolerate
noise, preserve live health control, and disclose that keeper measurements do
not establish application-level gains.

## Intent and verification

1. Compare one candidate with the current allocation using alternating ABA/BAB
   pairs and repeated, bounded evidence. Verify drift, changed conditions,
   invalid/partial results, noise and gains cannot create false confirmations.
2. Make bulk samples large enough to reduce start-up bias, distinguish warm-up
   and body timing, bound data/time, and reject short/partial samples. Test
   exact payload/stream budgets, warm-up exclusion and cancellation.
3. Stop on busy traffic, missing counters or adverse local gateway response;
   ensure probes never block leg-health reconciliation or alter selectors.
   Verify idle and loaded guards, counter resets, mode/health changes and the
   interaction between a running scan and hard failure.
4. Present the paired evidence, caveats and budgets honestly; retain manual
   pins and existing auto-mode failover. Run the performance race, Python and
   build gates on the exact resulting commit, then preserve evidence.

## Limits
Even a clean keeper comparison is synthetic and does not prove that arbitrary
TUN destinations, real connection/byte shares, uploads or user experience gain.
The safeguards trade faster adaptation for fewer unwarranted changes.
