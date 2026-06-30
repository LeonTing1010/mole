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

// State is the supervisor's live view of the connection, persisted so that
// `mole status` can read it without IPC.
type State struct {
	Mode             Mode      `json:"mode"`
	PID              int       `json:"pid"`
	SingboxPID       int       `json:"singbox_pid,omitempty"`
	Server           string    `json:"server,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	LastProbeAt      time.Time `json:"last_probe_at,omitempty"`
	LastLatencyMs    int       `json:"last_latency_ms,omitempty"`
	LastProbeError   string    `json:"last_probe_error,omitempty"`
	ConsecutiveFails int       `json:"consecutive_fails"`
	ConsecutiveOK    int       `json:"consecutive_ok"`

	// Keepalive fields track the in-tunnel keepalive that drives a tiny request
	// through the proxy outbound to keep the hy2/QUIC session and its NAT
	// mapping warm. These are informational only — the proxy health verdict
	// (LastProbe*) comes solely from the direct-UDP probe, which is immune to
	// the DNS hiccups this in-tunnel path can hit.
	LastKeepaliveAt    time.Time `json:"last_keepalive_at,omitempty"`
	LastKeepaliveMs    int       `json:"last_keepalive_ms,omitempty"`
	LastKeepaliveError string    `json:"last_keepalive_error,omitempty"`

	// Time-of-day bandwidth scheduler: which Brutal ceiling is currently selected.
	// Empty when the active server has no peak profile configured.
	BandwidthProfile  string `json:"bandwidth_profile,omitempty"`
	BandwidthDownMbps int    `json:"bandwidth_down_mbps,omitempty"`
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
