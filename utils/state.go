package utils

import (
	"encoding/json"
	"os"
	"time"
)

// Mode is the supervisor's current routing mode. There is only one mode today:
// foreign traffic always routes through the VPS. The supervisor probes VPS
// health for `mole status` reporting but does not reroute on failure.
type Mode string

const (
	ModeProxy Mode = "proxy" // foreign traffic → VPS
)

// ProbeVerdict classifies what one direct-UDP probe observed. Produced by
// core.ProbeHy2UDP; declared here so Health() can reason about it without an
// import cycle.
type ProbeVerdict string

const (
	ProbeAlive   ProbeVerdict = "alive"   // server answered the QUIC VN elicitation
	ProbeRefused ProbeVerdict = "refused" // ICMP port unreachable — hy2 process dead
	ProbeSilent  ProbeVerdict = "silent"  // no response — path blackholed or host gone
	ProbeError   ProbeVerdict = "error"   // probe couldn't run; says nothing about the VPS
)

// State is the supervisor's live view of the connection, persisted so that
// `mole status` can read it without IPC.
type State struct {
	Mode             Mode         `json:"mode"`
	PID              int          `json:"pid"`
	SingboxPID       int          `json:"singbox_pid,omitempty"`
	Server           string       `json:"server,omitempty"`
	StartedAt        time.Time    `json:"started_at"`
	LastProbeAt      time.Time    `json:"last_probe_at,omitempty"`
	LastLatencyMs    int          `json:"last_latency_ms,omitempty"`
	LastProbeError   string       `json:"last_probe_error,omitempty"`
	LastProbeVerdict ProbeVerdict `json:"last_probe_verdict,omitempty"`
	ConsecutiveFails int          `json:"consecutive_fails"`
	ConsecutiveOK    int          `json:"consecutive_ok"`

	// Keepalive fields track the in-tunnel keepalive that drives a tiny request
	// through the proxy outbound to keep the hy2/QUIC session and its NAT
	// mapping warm. A single failure is informational (this path rides sing-box
	// DNS and can hiccup), but KeepaliveFails ≥ KeepaliveFailThreshold flips
	// Health() to HealthPathDead: the direct-UDP probe only proves the VPS
	// process answers, not that the UDP path end-to-end passes traffic.
	LastKeepaliveAt    time.Time `json:"last_keepalive_at,omitempty"`
	LastKeepaliveMs    int       `json:"last_keepalive_ms,omitempty"`
	LastKeepaliveError string    `json:"last_keepalive_error,omitempty"`
	// KeepaliveFails counts consecutive keepalive failures; KeepaliveFailingSince
	// is when the current streak began (zero when healthy), so status can say how
	// long the path has been dark.
	KeepaliveFails        int       `json:"keepalive_fails,omitempty"`
	KeepaliveFailingSince time.Time `json:"keepalive_failing_since,omitempty"`

	// Time-of-day bandwidth scheduler: which Brutal ceiling is currently selected.
	// Empty when the active server has no peak profile configured.
	BandwidthProfile  string `json:"bandwidth_profile,omitempty"`
	BandwidthDownMbps int    `json:"bandwidth_down_mbps,omitempty"`
}

// Health is the synthesized verdict over both probes. The two measure
// different things and only together tell the truth:
//   - the direct-UDP probe (LastProbe*/ConsecutiveFails) runs OUTSIDE the
//     tunnel (interface-bound, QUIC VN elicitation) and can distinguish a
//     dead hy2 process (refused) from a dark path (silent) from health
//     (server responds);
//   - the in-tunnel keepalive (Keepalive*) proves the established hy2
//     session end-to-end actually passes traffic.
//
// Probe refused → HealthVPSDown (immediate — ICMP unreachable is
// unambiguous). Probe failing ≥ ProbeSilentThreshold, or keepalive failing
// ≥ KeepaliveFailThreshold, → HealthPathDead (path blackhole: wait it out or
// `mole down`; restarting does nothing). Otherwise → HealthOK.
type Health string

const (
	HealthOK       Health = "healthy"
	HealthVPSDown  Health = "vps-down"
	HealthPathDead Health = "path-blackhole"
)

// KeepaliveFailThreshold is how many consecutive keepalive failures flip
// Health to HealthPathDead. At the 15s default cadence, 4 ≈ one minute dark —
// enough to ride out a transient DNS hiccup, short enough to be honest during
// the typical 10–15min blackhole.
const KeepaliveFailThreshold = 4

// ProbeSilentThreshold is how many consecutive direct-probe failures flip
// Health to HealthPathDead. Single probe datagrams do get lost on this lossy
// CN path; three in a row (30s at the 10s cadence) is not luck.
const ProbeSilentThreshold = 3

// Health synthesizes the current verdict; see the Health type for semantics.
func (s *State) Health() Health {
	if s.ConsecutiveFails > 0 && s.LastProbeVerdict == ProbeRefused {
		return HealthVPSDown
	}
	if s.ConsecutiveFails >= ProbeSilentThreshold {
		return HealthPathDead
	}
	if s.KeepaliveFails >= KeepaliveFailThreshold {
		return HealthPathDead
	}
	return HealthOK
}

// WriteState atomically persists the state to ~/.mole/state.json.
func WriteState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := StatePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadState loads the last persisted state, if any.
func ReadState() (*State, error) {
	data, err := os.ReadFile(StatePath())
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RemoveState deletes the state file; called on clean shutdown.
func RemoveState() error { return os.Remove(StatePath()) }
