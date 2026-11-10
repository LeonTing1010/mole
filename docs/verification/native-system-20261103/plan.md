# Plan: exploit measured native host capabilities

Goal: improve actual same-node download with the existing M5/macOS and sing-box
1.14.3. Preserve authenticated protocol operation, operator pins, 400 Mbps rate,
DNS cache, kill switch, lid policy and unrelated dirty source.

1. Observe hardware/radio/power/sysctls and consult pinned primary sources.
   Verify already-enabled offload/TFO and avoid treating hardware link rate as
   delivered speed. Current machine is on battery with low-power mode enabled.
2. Correct the independent full-TUN harness: native DNS creates an owned address
   in 198.19.254.0/24; route only that address to the owned utun30. No binding of
   the application socket to a utun interface and no global default change.
   Prove payload and TLS operation before measuring kernel/mixed versus gvisor.
3. Compare stack / MTU on equal bytes and flows in alternating order, recording
   completed bytes and engine CPU time. Inspect production ENOBUFS and stop on
   regression. Only repeat comparisons justified by observed candidate gains.
4. Independently compare temporary low-power and TCP auto-buffer profiles, with
   unconditional restoration to observed values, including on interrupts.
5. Promote only repeatable gains with source and real protocol gates. Any product
   behavior change gets a failing constraint before implementation, isolated
   source patch, full project gates, rollback binary and runtime readback.

All exploratory scripts run before source modifications. No new VPS, kernel
extension, security bypass, engine upgrade or application system-proxy change.
Save negative/inconclusive results. An administrator authentication dialog may be
used for the isolated interface and temporary reversible OS controls.
