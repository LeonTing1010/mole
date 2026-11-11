package core

import "fmt"

// RateGovernor moves a declared TCP Brutal rate toward what this client can
// actually pull through the tunnel.
//
// TCP Brutal is deliberately not adaptive. It paces at the rate it is told and
// never probes for more, which is exactly what makes it win on long, lossy
// paths: no slow-start climb, and path loss is not mistaken for congestion. The
// price is that a stale declaration is never corrected by the protocol itself.
//
// For a laptop talking to a VPS the bottleneck is usually the side the node
// cannot observe — Wi-Fi, a congested uplink, a tethering quota. The node sees
// 8 multiplexed streams arriving through a tunnel and cannot attribute loss to
// any of them, so the only place a correction can be measured is on the client,
// where the bottleneck sits.
//
// The decision bands are deliberately asymmetric:
//
//   - Below collapseRatio the declaration is far above the path. Brutal holds
//     that rate straight through loss, so the resulting standing queue is what
//     makes even a single stream unusable. Act immediately and cut hard.
//   - A milder shortfall has to persist for two samples; path jitter routinely
//     produces one bad reading.
//   - Climbing needs two consecutive samples that saturate the declared rate, so
//     a transient lull in cross traffic is not mistaken for new headroom.
type RateGovernor struct {
	rate    int
	minRate int
	maxRate int

	hits   int // consecutive samples saturating the declared rate
	misses int // consecutive samples falling short of it

	// anchor makes the next measurement authoritative in one step. It is set by
	// Reset, where the previous declaration describes a different path and the
	// usual bands would take far too long to find the new one.
	anchor bool
}

const (
	// govCollapseRatio is the "this declaration is fiction" threshold. Only
	// below it does the loop act on a single sample, because at that point a
	// standing queue is already what the user feels. A dip that merely lands
	// between this and govLowRatio is far more likely to be channel jitter than
	// a shrunken path, and cutting on it would chase noise downward.
	govCollapseRatio = 0.50
	// govCollapseScale is the cut applied below it. The cut is floored at
	// 1.5× the measured rate so a path that is merely slow does not get
	// declared down to nothing, while a declaration that is orders of magnitude
	// above reality (the 600-on-Wi-Fi case) still collapses fast.
	govCollapseScale = 0.25
	// govLowRatio is the milder shortfall threshold. A healthy Brutal path
	// delivers ~82-88% of its declaration (measured 2026-11-11: 330-357 of
	// 380-420 on the ICN REALITY leg), so 0.88 treated a saturated path as a
	// shortfall and cut it repeatedly; 0.80 leaves that natural gap neutral
	// while still acting on a real collapse.
	govLowRatio = 0.80
	// govLowScale is the cut applied once govLowStreak samples agree.
	govLowScale = 0.85
	// govHighRatio is the "we are saturating what we declared" threshold.
	govHighRatio = 0.95
	// govHighScale is the climb applied once govHighStreak samples agree.
	govHighScale = 1.12
	// Streaks required in each direction before acting.
	govLowStreak  = 2
	govHighStreak = 2

	// govFarCeilRatio is how far below the ceiling counts as "far" (three
	// quarters of it). Far below, one saturating sample earns a big step: the
	// loop cannot discover headroom any other way, since a probe taken at the
	// declared rate can never measure more than that rate, and a cautious climb
	// from a collapsed declaration takes hours to find the path again.
	govFarCeilRatio  = 3 // rate*4 < ceiling*3
	govFarClimbScale = 1.35
	// govAnchorScale keeps a small margin over the measurement taken right after
	// a link change.
	govAnchorScale = 1.15
)

// NewRateGovernor seeds the loop at initial and clamps it to [minRate,maxRate].
// A non-positive maxRate means unbounded above.
func NewRateGovernor(initial, minRate, maxRate int) *RateGovernor {
	g := &RateGovernor{rate: initial, minRate: minRate, maxRate: maxRate}
	g.rate = g.clamp(initial)
	return g
}

// Rate is the rate the governor currently wants declared.
func (g *RateGovernor) Rate() int { return g.rate }

// Observe feeds one measured achievable rate (Mbps) to the loop. It returns the
// (possibly unchanged) rate, whether it moved, and a short reason for logging.
// A non-positive measurement means the probe failed rather than that the path is
// dead, so it is ignored: the supervisor's health machine owns "dead".
func (g *RateGovernor) Observe(achievedMbps float64) (int, bool, string) {
	if achievedMbps <= 0 || g.rate <= 0 {
		return g.rate, false, ""
	}
	ratio := achievedMbps / float64(g.rate)

	if g.anchor {
		g.anchor = false
		g.hits, g.misses = 0, 0
		if ratio >= govHighRatio {
			return g.rate, false, ""
		}
		return g.move(int(achievedMbps*govAnchorScale)+1,
			fmt.Sprintf("link changed — anchored to %.1f", achievedMbps))
	}

	switch {
	case ratio < govCollapseRatio:
		g.hits, g.misses = 0, 0
		next := int(float64(g.rate) * govCollapseScale)
		// Floor at 1.5× the measurement, but never above the current
		// declaration — this band means "we are under-delivering", so the new
		// rate has to be a drop.
		if floor := int(achievedMbps * 1.5); floor > next && floor < g.rate {
			next = floor
		}
		return g.move(next,
			fmt.Sprintf("measured %.1f of ↓%d — declaring far above the path", achievedMbps, g.rate))

	case ratio < govLowRatio:
		g.misses++
		g.hits = 0
		if g.misses >= govLowStreak {
			g.misses = 0
			return g.move(int(float64(g.rate)*govLowScale),
				fmt.Sprintf("measured %.1f of ↓%d twice", achievedMbps, g.rate))
		}

	case ratio >= govHighRatio:
		g.misses = 0
		if g.farBelowCeiling() {
			g.hits = 0
			return g.move(int(float64(g.rate)*govFarClimbScale)+1,
				fmt.Sprintf("saturated ↓%d far below the ceiling (%.1f)", g.rate, achievedMbps))
		}
		g.hits++
		if g.hits >= govHighStreak {
			g.hits = 0
			return g.move(int(float64(g.rate)*govHighScale)+1,
				fmt.Sprintf("saturated ↓%d twice (%.1f)", g.rate, achievedMbps))
		}

	default:
		g.hits, g.misses = 0, 0
	}
	return g.rate, false, ""
}

// Reset re-declares the ceiling and trusts the next measurement to correct it in
// one step.
//
// This is for the case the bands handle badly: the local link changed (Wi-Fi to
// wired, a 2.4GHz roam to 5GHz), so the previous declaration describes a
// different path entirely. Climbing there by increments would take hours, since
// a probe can never measure more than the rate currently declared. Declaring the
// ceiling overshoots on purpose, and the immediate sample is taken as
// authoritative instead of waiting a full interval.
func (g *RateGovernor) Reset(ceiling int) {
	g.rate = g.clamp(ceiling)
	g.hits, g.misses, g.anchor = 0, 0, true
}

// farBelowCeiling reports whether the declaration is far enough below the
// operator's best-case statement to justify a single-sample climb.
func (g *RateGovernor) farBelowCeiling() bool {
	return g.maxRate > 0 && g.rate*4 < g.maxRate*govFarCeilRatio
}

func (g *RateGovernor) move(next int, reason string) (int, bool, string) {
	next = g.clamp(next)
	if next == g.rate {
		return g.rate, false, ""
	}
	g.rate = next
	return next, true, reason
}

func (g *RateGovernor) clamp(n int) int {
	if n < g.minRate {
		return g.minRate
	}
	if g.maxRate > 0 && n > g.maxRate {
		return g.maxRate
	}
	return n
}
