# Speed sampling repair — 2026-11-11

The installed executable reports revision 68e797e. It does not include the prior
1ee32be audit fixes. No system executable, running tunnel, server rate, gain,
transport selection or credentials were changed in this task.

## Implemented

Removed the 100 ms minimum duration from bulk measurement. A capped 6 MiB
governor sample takes less than 100 ms at about 503 Mbps or more, so valid fast
samples were discarded. A successful HTTP response and exactly the requested
number of bytes now determine validity. Failed reads and wrong-sized payloads
still fail, and non-positive durations are rejected.

The client-taking internal helper allows deterministic tests without a socket.
The public API still creates fresh connections and imposes a 20 second timeout.
No sampling-frequency, packet-pacing or concurrency policy was changed without
live evidence.

## Verification and execution

- RED: complete in-memory sub-100-ms sample reproduced the old rejection.
- GREEN: targeted Go race tests cover valid fast samples and HTTP failure, short
  body, excess body, read error and dial error; governor regression tests pass.
- `go vet ./...` and macOS candidate build pass.
- All 11 Python tests pass.
- Full `scripts/verify-performance.sh` was attempted. It is BLOCKED by sandbox
  `bind: operation not permitted` errors in socket-based existing tests. It is
  not a passing release gate; see full-gate-blocked.log.
- Live baseline used 8 connections, 5 seconds and a 32 MiB budget per phase
  (96 MiB maximum across three phases). Local proxy connections were denied
  with `Operation not permitted`; the first phase transferred no payload and
  the script stopped. Its zero rate is NOT a capacity measurement. See
  baseline-blocked.json.
- `/usr/local/bin/mole` and its parent directory are not writable in this
  environment. No installation or daemon restart was performed.

## Still required

Run the full verification gate in an environment allowing localhost sockets,
then install the verified executable and restart the daemon. Measure both legs
and their concurrent combination before changing rate/gain/concurrency or the
sampling interval. No throughput improvement has been measured or claimed.

## Persistence

Git staging is blocked: creating the worktree index.lock returns Operation not
permitted. The patch and tests are preserved as uncommitted changes in the
persistent main project directory. No new commit, merge or push was completed.

The task's temporary worktree files were restored/removed only after their
persistent copies were byte-verified. Git removed the temporary directory but
could not delete its worktree metadata because .git is read-only; the stale
registration is left untouched for later authorized cleanup. Existing worktree
registrations and ignored user files were not changed.
