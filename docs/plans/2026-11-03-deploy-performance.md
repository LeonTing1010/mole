# Plan: shorten required node installation

## Context

The current deployment took 131 seconds: 18 seconds through address assignment,
then 113 seconds until both external authenticated HTTPS checks succeeded.
The latter interval includes cloud boot, installation and verification; remote
stage timings were not observed. The generated script currently runs HY2,
including optional service cleanup, before starting REALITY installation.

## Goal

Allow both required protocol installations to progress independently after SSH
recovery access is installed. Start HY2 before optional cleanup. Preserve pinned
downloads, package lock handling, the shared deadline and external readiness.

## Verification conditions

- The generated dual boot script reaches both installers before either is
  released by a barrier, without wall-clock speed assertions.
- Either installer can fail while the other still starts its required service.
- SSH access is prepared before either installer starts.
- Optional service cleanup observes HY2 already running.
- Both installers retain the same absolute deadline.
- Full Go race tests, Python tests, vet and the macOS build pass.

## Tasks

1. RED: exercise generated scripts in a filesystem/command sandbox and show
   current sequential installation cannot pass the barrier.
2. GREEN: share the HY2 fragment between standalone and dual scripts; launch
   required fragments concurrently, join both children, move cleanup after
   HY2 activation, and emit timestamped required-stage events.
3. Verify, inspect the delta against the dirty baseline, integrate only named
   changes and build a separate executable. Do not restart the working VPN.

## Limits

Local script verification does not establish faster cloud boot or a new
end-to-end deployment duration. A fresh billable cloud instance is not created
by this work. No live throughput setting is raised from a short speed sample.
