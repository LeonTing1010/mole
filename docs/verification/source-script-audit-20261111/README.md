# Source/script audit repair — 2026-11-11

Scope: local source and deployment assets. No live VPS, host DNS/PF state, running
tunnel, installed CLI, cloud account, or remote Git reference was changed.

## Fixed failure modes

- HTTP errors, short bodies and truncated bodies can no longer qualify as bulk
  throughput. Invalid stream/payload inputs fail before spawning work.
- Gain changes require a bounded response confirming the requested gain and a
  positive applied count; failed final application/verification returns an error.
- Named tuning must match the running daemon's server.
- Omitted gain retains the current gain; standalone default-rate/gain environment
  variables remain supported after keeper consolidation.
- A stale sing-box PID must match a current sing-box process and the exact config
  argument. Config names ending in `.bak` cannot match the original path.
- Configured governor ceilings apply even at/below the seed. The first declaration
  uses the clamped value; very low ceilings win over the usual minimum rate.
- CLI, Docker and cloud-init share one keeper and one pinned/checksummed engine
  installer. Cloud-init is reproducibly generated from checked-in assets.
- Standalone identity generation persists credentials, derives the correct X25519
  public key, serializes configs safely and emits private client-link files.
- Standalone REALITY uses accepted sing-box TLS fields; Hysteria owns UDP/443 and
  has the same salamander credentials as its client URI. Service permissions allow
  certificate access while identity/client secrets remain mode 0600.
- Docker invokes the installed hysteria path, includes required runtime tools,
  propagates child failures and handles TERM/INT while reaping siblings.
- Cloud-init begins with its required header, materializes real credentials, checks
  service activation and treats the optional kernel module separately.
- The documented installer target now exists; failed download returns failure.
  Import/adopt documentation matches the implemented CLI.

## Verification

- Regression tests were observed failing before the corresponding implementation:
  malformed bulk responses, unrelated recycled PID, governor ceilings, omitted
  gain, wrong-node tune, unapplied gain, broken binary bulk, missing shared config /
  cloud generator, restrictive certificate directory, config-path prefix matching,
  and standalone default environment compatibility.
- `go vet ./...`: passed.
- `bash scripts/verify-performance.sh /tmp/mole-audit-verified-20261111`: passed
  after the core fixes, including all Go race tests, 11 Python tests and macOS build.
- Final keeper/default and standalone-link additions: targeted Go race tests,
  Python suite, vet and macOS build passed again.
- `GOOS=linux GOARCH=amd64 go build ...`: passed for the CLI.
- Shell syntax: every checked-in deploy shell script, install redirect, and scripts
  shell entrypoint passed `bash -n` individually.
- Generated sing-box config passed local `sing-box check` (installed engine 1.14.3).
  CLI import was tested against generated links, including an independent Go
  X25519 public-key calculation.
- Cloud-init generation is compared byte-for-byte with the checked-in artifact.
- Container shutdown is exercised with sandbox child processes for failure and
  SIGTERM; the test verifies sibling PIDs are gone.

## Remaining environment-dependent acceptance

Docker client is installed but its daemon socket is absent. A real Docker image
build/run was not performed. No paid/cloud VM was provisioned and no real kernel
module or systemd boot was exercised. The pinned server engine is 1.14.4; local
configuration checks used 1.14.3. A Linux host with Docker/systemd and the pinned
artifacts is needed for those deployment acceptance checks.

The patch consolidates substantial duplicate scripts, so its raw file/line count
includes moved code and generated cloud-init. No generated credentials or test
identities are committed. Remote installer URLs become usable once these changes
are pushed; this task performs local commits only.
