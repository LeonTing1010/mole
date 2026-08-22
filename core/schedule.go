package core

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProxySelectorTag is the tag the route always targets. With a Brutal ceiling in
// play, config.Build emits it as a `selector` over a LADDER of otherwise-identical
// hy2 outbounds that differ only in their declared bandwidth; without one it is a
// single plain outbound. Routing and the Clash keepalive reference this tag either
// way, so nothing downstream cares which shape is in force.
const ProxySelectorTag = "proxy"

// BandwidthRungTag names the ladder member carrying a given DOWN ceiling.
//
// The ladder exists because an outbound's up_mbps/down_mbps is immutable after
// sing-box loads its config — the Clash API can switch which outbound a selector
// points at, but it cannot mutate one. So every ceiling we might want to reach at
// runtime has to be pre-materialized as its own outbound. Members are dialed
// lazily (a rung holds no QUIC session until something routes to it or pre-warms
// it), so an unused rung costs a few lines of JSON and nothing else.
func BandwidthRungTag(down int) string { return fmt.Sprintf("proxy-bw-%d", down) }

// BandwidthRungMbps reads the ceiling back out of a ladder member's tag, so a
// live selector answer can be reported as a number. Returns 0 for anything that
// is not a rung — the selector may legitimately be pointing at a member the
// ladder did not mint, and guessing a ceiling for it would be worse than saying
// nothing.
func BandwidthRungMbps(tag string) int {
	rest, ok := strings.CutPrefix(tag, "proxy-bw-")
	if !ok {
		return 0
	}
	down, err := strconv.Atoi(rest)
	if err != nil || down <= 0 {
		return 0
	}
	return down
}

// standardRungs are the ceilings the ladder always offers when they fit under
// the configured maximum. Roughly 1.5×-spaced up to 50: fine enough to track a
// link that swings by an order of magnitude, coarse enough that switching is a
// decision rather than a twitch.
//
// The spacing tightens above 50 on purpose. Down low, picking a rung too small
// only leaves throughput unused, so wide steps cost little. Up high you are
// closing in on the physical link, where the two directions stop being
// symmetric: one rung short is a few Mbps unused, one rung long is Brutal
// declaring more than the path can carry and flooding it into ENOBUFS. Near the
// measured cap, step carefully.
var standardRungs = []int{1, 2, 3, 5, 8, 12, 20, 30, 50, 65, 80, 100}

// Default Brutal ceiling for a server that carries no explicit one. Lives here
// because both the config builder and the server list must resolve "unset" to
// the same numbers — when they disagreed, the config laddered from one ceiling
// while the supervisor scheduled over another, and the tunnel sat on the top
// rung with nothing able to move it down.
//
// Deliberately conservative: Brutal ignores loss by design, so a ceiling above
// the real link floods it into repeated collapse, while one below only leaves
// throughput unused. Raise it per server with `mole ceiling` once measured.
const (
	DefaultUpMbps       = 5
	DefaultDownMbps     = 20
	DefaultPeakUpMbps   = 2
	DefaultPeakDownMbps = 5
)

// BandwidthSchedule encodes a time-of-day Brutal ceiling for the hy2 outbound.
//
// Why this exists: hysteria2's Brutal congestion control holds a FIXED declared
// send rate and ignores packet loss by design. On a cross-border path whose
// available bandwidth swings by time of day (e.g. CN→NRT measured ~30 Mbps
// off-peak but ~2.5 Mbps at peak), no single fixed ceiling fits — set it high
// and Brutal floods the throttled path at peak (ENOBUFS + retransmit storms +
// "no recent network activity" stalls); set it low and off-peak throughput is
// needlessly capped. BBR (no declared bandwidth) is not the answer either: it is
// loss-sensitive and collapses to tens of KB/s on this lossy path at peak.
//
// The fix is to track the link: run a low ceiling during the peak window and a
// high one off-peak. The supervisor selects the matching outbound on a timer.
type BandwidthSchedule struct {
	OffUp, OffDown   int // off-peak ceiling (the server's normal up_mbps/down_mbps)
	PeakUp, PeakDown int // peak-window ceiling
	StartHour        int // local-clock peak window start hour [0,24)
	EndHour          int // local-clock peak window end hour [0,24); wraps past midnight
}

// Enabled reports whether a peak profile is configured. With no peak ceiling the
// schedule is inert and the supervisor never flips — behaviour is identical to a
// plain fixed-bandwidth server.
func (s BandwidthSchedule) Enabled() bool { return s.PeakDown > 0 }

// inPeak reports whether local time t falls inside the peak window. A window
// where StartHour > EndHour wraps past midnight (e.g. 12→2 means 12:00–02:00).
func (s BandwidthSchedule) inPeak(t time.Time) bool {
	if s.StartHour == s.EndHour {
		return false
	}
	h := t.Hour()
	if s.StartHour < s.EndHour {
		return h >= s.StartHour && h < s.EndHour
	}
	return h >= s.StartHour || h < s.EndHour
}

// Profile returns the profile name and the (up, down) Brutal ceiling that apply
// at local time t. When the schedule is disabled it always returns the off-peak
// ceiling so callers can use it unconditionally.
func (s BandwidthSchedule) Profile(t time.Time) (name string, up, down int) {
	if s.Enabled() && s.inPeak(t) {
		return "peak", s.PeakUp, s.PeakDown
	}
	return "offpeak", s.OffUp, s.OffDown
}

// Member returns the selector member tag to route through at local time t.
func (s BandwidthSchedule) Member(t time.Time) string {
	_, _, down := s.Profile(t)
	return BandwidthRungTag(down)
}

// Rungs returns the ladder's DOWN ceilings, ascending. It always contains the
// ceilings the clock can select (off-peak, and peak when configured) so the
// scheduler can never name a member the config lacks; the standard rungs fill in
// between so a manual pin has somewhere to land. Returns nil when no Brutal
// ceiling is configured (OffDown<=0, i.e. BBR mode) — there is nothing to ladder.
func (s BandwidthSchedule) Rungs() []int {
	if s.OffDown <= 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	add := func(n int) {
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// The clock-reachable ceilings go in unconditionally — Member() can name
	// either of them, so a config missing one would leave the scheduler selecting
	// an outbound that does not exist. This holds even in the odd case of a peak
	// ceiling above the off-peak one.
	add(s.OffDown)
	add(s.PeakDown)
	// Standard rungs only fill in *below* the configured maximum; offering more
	// headroom than the operator measured would invite pinning a ceiling the link
	// was never shown to sustain, which is how Brutal floods.
	max := s.OffDown
	if s.PeakDown > max {
		max = s.PeakDown
	}
	for _, n := range standardRungs {
		if n < max {
			add(n)
		}
	}
	sort.Ints(out)
	return out
}

// StepDown returns the ceiling `steps` rungs below `down`, clamped to the
// lowest rung the ladder offers. A `down` that is not itself a rung snaps to the
// nearest rung at or below it first, so a stale pin can't wander off the ladder.
//
// This is what the supervisor's self-healing walks: a Brutal ceiling above what
// the path can carry floods it, and the remedy is always to descend, never to
// climb. Returns 0 when there is no ladder.
func (s BandwidthSchedule) StepDown(down, steps int) int {
	rungs := s.Rungs()
	if len(rungs) == 0 {
		return 0
	}
	i := 0
	for j, n := range rungs {
		if n <= down {
			i = j
		}
	}
	i -= steps
	if i < 0 {
		i = 0
	}
	return rungs[i]
}

// Sibling returns a rung adjacent to `down`, or 0 when the ladder has nothing
// else to offer.
//
// This is not a bandwidth decision — it is how the supervisor gets a FRESH QUIC
// session. An outbound's session is not addressable through the Clash API: there
// is no "reconnect" call, only a selector that says which outbound carries
// traffic. So when the established session is the thing that has gone bad — the
// keepalive failing while a fresh out-of-tunnel probe still gets answers, which
// is per-five-tuple throttling — the only lever is to route through a different
// outbound, which dials its own session on its own source port.
//
// Prefers one rung DOWN, because the alternative is momentarily declaring more
// bandwidth than the rung that was already struggling. Falls back to one rung up
// when there is nothing below.
func (s BandwidthSchedule) Sibling(down int) int {
	rungs := s.Rungs()
	if len(rungs) < 2 {
		return 0
	}
	i := 0
	for j, n := range rungs {
		if n <= down {
			i = j
		}
	}
	if i > 0 {
		return rungs[i-1]
	}
	return rungs[i+1]
}

// HasRung reports whether the ladder carries a given DOWN ceiling — i.e. whether
// selecting it would name an outbound the config actually defines.
func (s BandwidthSchedule) HasRung(down int) bool {
	for _, n := range s.Rungs() {
		if n == down {
			return true
		}
	}
	return false
}

// RungUp returns the UP ceiling to pair with a given DOWN rung.
//
// The configured pairs win outright, so the clock-selected rungs keep exactly the
// ceilings the operator measured. Intermediate rungs have no measured partner, so
// they inherit the off-peak up:down ratio — hysteria2's Brutal wants both
// directions declared, and holding the ratio keeps a pinned rung proportionate
// rather than pairing, say, a 3 Mbps down with a 5 Mbps up. Never returns 0: a
// zero up_mbps would silently drop that rung out of Brutal and back into BBR.
func (s BandwidthSchedule) RungUp(down int) int {
	switch {
	case down == s.OffDown && s.OffUp > 0:
		return s.OffUp
	case down == s.PeakDown && s.PeakUp > 0:
		return s.PeakUp
	}
	if s.OffUp > 0 && s.OffDown > 0 {
		if up := down * s.OffUp / s.OffDown; up > 0 {
			return up
		}
	}
	return 1
}
