package core

import (
	"fmt"
	"sort"
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

// standardRungs are the ceilings the ladder always offers when they fit under
// the configured maximum. Roughly 1.5×-spaced: fine enough to track a link that
// swings by an order of magnitude, coarse enough that switching is a decision
// rather than a twitch.
var standardRungs = []int{1, 2, 3, 5, 8, 12, 20, 30, 50}

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
