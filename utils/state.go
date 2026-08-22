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
	// ProbeNoLink: the probe could not run because this machine currently has
	// no physical interface carrying an IPv4 address. Split out of ProbeError
	// because it is the one probe failure that is definitely NOT about the VPS
	// or the path — the local uplink is gone, and nothing at the far end can be
	// measured, blamed, or fixed until it comes back.
	//
	// It has its own verdict because it has its own remedy (wait for DHCP, then
	// re-dial), and because folding it into the path verdict actively hurt: a
	// 60-second Wi-Fi re-association was logged as "path-blackhole", counted as
	// unhealthy by self-healing, and threw away the clean streak the ceiling
	// needed to climb back.
	ProbeNoLink ProbeVerdict = "no-link"
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

	// Whether the running daemon's destroy-on-lid-close watcher is actually
	// armed, and why not when it isn't. Recorded because the feature used to be
	// silent in both directions: armed logged nothing, and disarmed printed one
	// warning to the terminal of the `mole up` that started it — which scrolls
	// away, never reaches mole.log, and is gone entirely by the time anyone
	// wonders whether last night's lid close should have deleted the server.
	// Empty in state written by an older daemon; callers must treat that as
	// "unknown" rather than "off".
	LidDestroy       LidDestroyState `json:"lid_destroy,omitempty"`
	LidDestroyReason string          `json:"lid_destroy_reason,omitempty"`

	// The debt left by a destroy that fired and failed: when the lid shut, and
	// how many times we have tried since. Set only in the Pending and Cancelled
	// states, where they are the two numbers anyone actually wants — how long
	// the server has been billing for nothing, and whether anything is still
	// trying. Zero everywhere else.
	LidDestroyPendingSince time.Time `json:"lid_destroy_pending_since,omitempty"`
	LidDestroyAttempts     int       `json:"lid_destroy_attempts,omitempty"`
}

// LidDestroyState is what the daemon will do when the lid is shut.
type LidDestroyState string

const (
	// LidDestroyArmed: the watcher is running and holds a usable Vultr key.
	LidDestroyArmed LidDestroyState = "armed"
	// LidDestroyDryRun: the watcher is running but will only log the lid close.
	LidDestroyDryRun LidDestroyState = "dry-run"
	// LidDestroyOff: nothing is watching; the server bills until destroyed.
	LidDestroyOff LidDestroyState = "off"
	// LidDestroyFired: the destroy already happened — the server is gone and
	// the daemon is stopping (or was killed mid-shutdown). Kept in the same
	// field so `mole status` answers "what happened to my server?" from the
	// same read it already makes, instead of showing a frozen "tunnel dark"
	// through a server that no longer exists.
	LidDestroyFired LidDestroyState = "fired"
	// LidDestroyPending: the lid shut, the destroy fired, and it failed — the
	// network was down at the one moment it had to work. The server is still
	// running and still billing.
	//
	// This is a debt, not an error, and that is why it is a state rather than a
	// log line. On 2026-08-09 the lid closed at 04:45 into a dead uplink, the
	// DELETE spent its whole 20s budget timing out, and nothing tried again:
	// the machine dark-woke twenty-odd times before morning with the daemon
	// running the whole while, and the server billed 5h20m for nobody. The
	// daemon keeps trying to pay this off on every wake that finds the lid
	// still shut.
	LidDestroyPending LidDestroyState = "pending"
	// LidDestroyCancelled: the lid was opened before the debt was paid. Their
	// owner is back and wants the tunnel, so the server is deliberately left
	// alone — destroying it now would only mean redeploying it a minute later,
	// and Vultr bills a fresh hour either way. Recorded rather than forgotten
	// because the hours between the lid close and the lid open were paid for
	// and nobody was there to use them.
	LidDestroyCancelled LidDestroyState = "cancelled"
)

// Health is the synthesized verdict over both probes. The two measure
// different things and only together tell the truth:
//   - the direct-UDP probe (LastProbe*/ConsecutiveFails) runs OUTSIDE the
//     tunnel (interface-bound, QUIC VN elicitation) and can distinguish a
//     dead hy2 process (refused) from a dark path (silent) from health
//     (server responds);
//   - the in-tunnel keepalive (Keepalive*) proves the established hy2
//     session end-to-end actually passes traffic.
//
// The two disagree more often than they agree, and which one is failing is the
// whole diagnosis:
//
//   - probe silent, keepalive fine → HealthDegraded. The probe opens a NEW
//     five-tuple with a 1200-byte datagram every time; on this path ~20% of
//     those first packets are lost even while the tunnel carries 50 Mbps
//     (measured 2026-08-22: 30 probes, 24 answered, tunnel healthy throughout).
//     Three consecutive losses is a 0.8% event — unremarkable over a day, and it
//     used to flip the verdict to path-blackhole 21 times, each one clearing
//     self-healing's clean streak and painting `mole status` red through a
//     working tunnel.
//   - keepalive failing → HealthPathDead, whatever the probe says. This is the
//     failure users actually feel: established flows stop passing traffic while
//     new ones are fine, which is the signature of per-five-tuple throttling and
//     shows up as ~30s outages (hysteria's MaxIdleTimeout) on an otherwise live
//     path.
//   - both failing → HealthPathDead, the genuine blackout.
//
// Probe refused → HealthVPSDown (immediate — ICMP unreachable is unambiguous).
// Probe reporting no local link → HealthNoLink, which is about this machine and
// not about the tunnel at all.
type Health string

const (
	HealthOK       Health = "healthy"
	HealthVPSDown  Health = "vps-down"
	HealthPathDead Health = "path-blackhole"
	// HealthDegraded: the out-of-tunnel probe is missing replies but the tunnel
	// itself is still passing traffic. Worth showing and worth refusing to raise
	// the ceiling on; not worth calling an outage.
	HealthDegraded Health = "degraded"
	// HealthNoLink: this machine has no usable uplink. Nothing about the VPS or
	// the path is known — or knowable — until it returns.
	HealthNoLink Health = "no-link"
)

// Usable reports whether the tunnel is expected to carry traffic right now.
// HealthDegraded is deliberately usable: that is the entire point of splitting
// it out of HealthPathDead.
func (h Health) Usable() bool { return h == HealthOK || h == HealthDegraded }

// KeepaliveFailThreshold is how many consecutive keepalive failures flip
// Health to HealthPathDead. At the 15s default cadence, 4 ≈ one minute dark —
// enough to ride out a transient DNS hiccup, short enough to be honest during
// the typical 10–15min blackhole.
const KeepaliveFailThreshold = 4

// ProbeSilentThreshold is how many consecutive direct-probe failures flip
// Health to HealthDegraded. Single probe datagrams do get lost on this lossy
// CN path — measured at ~20% for the probe's fresh five-tuple — so three in a
// row is a ~1-in-125 event rather than luck, but it is still only evidence
// about NEW flows. Promoting it to an outage is the keepalive's job.
const ProbeSilentThreshold = 3

// Health synthesizes the current verdict; see the Health type for semantics.
func (s *State) Health() Health {
	if s.ConsecutiveFails > 0 && s.LastProbeVerdict == ProbeRefused {
		return HealthVPSDown
	}
	// Checked before anything path-related: with no local IPv4 the probe cannot
	// run and the keepalive cannot reach the tunnel, so every other signal on
	// this struct is measuring the absence of an uplink, not the state of the
	// far end.
	if s.ConsecutiveFails > 0 && s.LastProbeVerdict == ProbeNoLink {
		return HealthNoLink
	}
	// The keepalive is the authority on whether the tunnel passes traffic: it
	// rides an established session end to end. When it is failing, the path is
	// dead no matter how healthy a fresh probe datagram looks.
	if s.KeepaliveFails >= KeepaliveFailThreshold {
		return HealthPathDead
	}
	if s.ConsecutiveFails >= ProbeSilentThreshold {
		return HealthDegraded
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
