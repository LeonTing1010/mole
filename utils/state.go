package utils

import (
	"encoding/json"
	"os"
	"time"
)

// Mode is the supervisor's current routing mode.
type Mode string

const (
	ModeProxy Mode = "proxy" // foreign traffic → VPS
	ModeBlock Mode = "block" // foreign traffic → black hole (VPS unreachable)
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
