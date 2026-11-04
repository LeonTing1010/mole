# Plan: qualify OpenAI connection latency and recovery

## Context

The user observed better networking after switching from 2.4 GHz to 5 GHz.
Completed same-budget samples improved, but OpenAI transport had no pre-switch
baseline. Current unauthenticated OpenAI API requests return 401, and ChatGPT
HEAD requests return 403. Neither measures logged-in model generation.
Production currently uses REALITY/Vision without multiplex and has historical
application errors dialing the node despite healthy sparse monitor samples.

## Goal

Locate measured setup, response and stability costs along the current path;
qualify reversible native-protocol candidates in isolated local engines; retain
raw private receipts and a sanitized source-linked analysis on a persistent Git
branch. Changes to production settings require evidence from matched trials.

## Verification conditions

- Current production configuration, server store, route/DNS ownership and daemon
  remain accounted for; existing unrelated work is protected.
- Separate DNS synthesis, node TCP reachability, authenticated tunnel delivery,
  remote HTTPS setup/response and application connection reuse.
- Every candidate preserves certificate validation and existing credentials.
- Compare identical origins, concurrency and byte budgets in rotated order;
  record failed attempts and tails rather than replacing them with zero.
- Separate edge/unauthenticated responses from authenticated application and
  streamed model delivery, which remain unobserved without direct evidence.
- Stop experiments on ENOBUFS or resource interference; remove task-owned local
  engines/configurations and keep credential-bearing files out of Git.

## Tasks

### 1. Establish current path and source contracts

- Intent: identify costs and failure boundaries before tuning.
- Verification: sanitized runtime inventory, timed DNS/gateway/node/HTTPS probes,
  time-aligned log classification, source references and primary documentation.

### 2. Qualify connection reuse and native transports

- Intent: reduce setup and tail latency without assuming higher bulk capacity
  makes model generation faster.
- Verification: fresh versus reused HTTP, REALITY versus native smux versus HY2
  on the same node, repeated small requests and bounded equal-budget downloads.
  Keep production routing and selectors unchanged.

### 3. Reproduce any proven behavioral defect before implementation

- Intent: fix only a demonstrated cause within this task's scope.
- Verification: declare a failing behavioral constraint, observe RED, implement
  the minimum fix, observe GREEN, and run the project verification script.
  If no defect is established, retain the negative result and avoid a patch.

### 4. Deliver an evidence-bound recommendation

- Intent: identify actionable gains and distinguish observed limits.
- Verification: durable private receipts, sanitized committed report, final
  runtime/working-tree preservation and removal of owned temporary resources.

## Risks

Wi-Fi interference and node egress can vary during serial measurements. Smux can
reuse a TCP session but also share its loss across streams. HY2 has a different
400 Mbps configured limit and UDP treatment. A valid TLS/401/403 response does
not establish login, streaming, request processing or model output speed.

## Final verification

For source changes use `bash scripts/verify-performance.sh <temporary-binary>`.
For diagnostic artifacts verify successful parsing, exact byte accounting,
failure retention, no secrets in tracked content, and production preservation.
