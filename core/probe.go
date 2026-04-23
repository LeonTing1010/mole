package core

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// ProbeHy2UDP checks whether the hy2/QUIC server at addr is reachable over
// UDP. Returns the round-trip delay if a response was received, or 0 with a
// nil error when the server silently absorbed the probe (the normal outcome
// for QUIC receiving a malformed packet).
//
// Why not just hit a URL through the proxy? Because that path runs through
// sing-box's DNS engine, and DoT to 1.1.1.1 — the configured upstream — is
// unreliable on many ISPs. A DNS hiccup would be misread as VPS death,
// flipping the supervisor into block mode and breaking the user's traffic
// while the VPS is in fact perfectly healthy. Probing the VPS endpoint
// directly removes DNS and Clash from the failure path; we measure exactly
// the thing we care about.
//
// Failure modes that DO surface here (correctly):
//   - net unreachable / no route                → caller's network is down
//   - ICMP port unreachable (kernel reports as
//     "connection refused" on read/write)       → hy2 process dead on VPS
//   - dial timeout                              → VPS or path totally dark
//
// Failure modes intentionally NOT surfaced (would be false positives):
//   - read timeout with no error                → server received & dropped
//     our garbage byte; this is what a healthy QUIC server does
func ProbeHy2UDP(addr string, timeout time.Duration) (time.Duration, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	if _, err := conn.Write([]byte{0}); err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}

	// Healthy hy2 silently drops our junk byte, so on success we hit the read
	// deadline. Make that deadline short — we only need enough time for an
	// ICMP-port-unreachable to round-trip from a dead server (≈one RTT). Long
	// read deadlines just burn wall-clock on every healthy probe and make the
	// reported "latency" look alarming in `mole status`.
	readDeadline := timeout / 4
	if readDeadline < 500*time.Millisecond {
		readDeadline = 500 * time.Millisecond
	}
	readStart := time.Now()
	if err := conn.SetReadDeadline(readStart.Add(readDeadline)); err != nil {
		return 0, err
	}
	buf := make([]byte, 64)
	_, err = conn.Read(buf)
	rtt := time.Since(readStart)
	if err == nil {
		return rtt, nil
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		// Silence — what a healthy QUIC server gives back. Report 0 so status
		// doesn't claim a fake latency.
		return 0, nil
	}
	return 0, fmt.Errorf("read: %w", err)
}
