package core

import "time"

// RealityLegTag names the selector member carrying the VLESS+REALITY leg.
//
// It lives in the SAME selector as the Brutal ladder rungs, which is the whole
// trick: moving traffic between two different transport protocols costs exactly
// what a ceiling change costs — one Clash API call, no sing-box restart, no
// rebuilt TUN, no dropped connections. See buildProxyOutbounds.
//
// Since 2026-09-04 this is the PRIMARY leg on any node that carries it, and the
// hy2 ladder is the fallback. The inversion was decided on four days of logs:
// 66 health failures on hy2 against zero on REALITY in every A/B, 87% of them
// the shape "new connections fail while established ones and single datagrams
// are fine" — an ISP treating outbound UDP flows, not a ceiling, not the host,
// not the route. On the clean path both legs measure the same (73–89 Mbps down
// on REALITY against hy2's 74 at its top rung; 34 Mbps up against 30), so the
// only thing hy2 still buys is Brutal on a lossy path — and that is exactly
// what the fallback is for.
const RealityLegTag = "proxy-reality"

const (
	// fallbackEnterFails is how many consecutive in-tunnel keepalive failures —
	// with the out-of-tunnel probe still answering — hand traffic from REALITY
	// to the hy2 ladder.
	//
	// At the 15s keepalive cadence that is ~90s. The probe is UDP to the same
	// ip:port, so "keepalive dark, probe alive" says the host and the route are
	// up and it is TCP/443 specifically that has stopped carrying. On the
	// fallback leg the same count in the other direction means hy2 is dark too,
	// and traffic goes back to the primary rather than staying on a leg that
	// was only ever a bet.
	fallbackEnterFails = 6

	// fallbackMinDwell is how long traffic stays on hy2 before REALITY is even
	// reconsidered. These episodes last minutes, not seconds; returning early
	// just spends another 90s of dark tunnel discovering that.
	fallbackMinDwell = 10 * time.Minute
	// fallbackMaxDwell caps the doubling below, so a bad night cannot strand the
	// tunnel on the fallback indefinitely once the path recovers.
	fallbackMaxDwell = 60 * time.Minute
	// fallbackProbation is how long a return has to survive to count as real.
	// Go back to the fallback sooner than this and the return was premature,
	// which doubles the next dwell.
	fallbackProbation = 3 * time.Minute

	// legTestInterval is the floor between comparative leg tests (see
	// Supervisor.considerLegFailover). The fresh-dial latch is a Schmitt
	// trigger with a 5% release, so it can legitimately stay set for the whole
	// length of an episode; on 2026-09-01 that was 98 minutes. Without a floor,
	// the "both legs are equally bad" answer would be re-measured every 15s for
	// all 98 of them.
	legTestInterval = time.Minute
)

// fallbackState folds the failover decision into a value with no dependencies,
// so the policy can be exercised directly. Same reasoning as healState: the
// condition it guards against takes a live QoS episode to reproduce, and that
// is exactly the kind of logic that otherwise only ever runs in production.
//
// `on` means traffic is on the hy2 fallback because the primary was measured
// failing. The clock arrives as a parameter rather than being read here, which
// is what makes a ten-minute dwell testable in microseconds.
type fallbackState struct {
	on    bool
	since time.Time
	// lastBack is when traffic was last handed back to the primary, used to
	// tell a return that held from one that did not.
	lastBack time.Time
	// dwell is how long THIS stay on the fallback lasts before a return is tried.
	dwell time.Duration
}

// enter moves traffic to the fallback leg, and reports whether this call is
// what changed it.
//
// The dwell doubles when the previous return failed probation. Without that, a
// path that is being throttled all evening pays the full detection cost — ~90s
// of dark tunnel — once per dwell, all evening. With it, mole notices the
// pattern and stops re-testing a path that keeps saying no.
func (b *fallbackState) enter(now time.Time) bool {
	if b.on {
		return false
	}
	next := fallbackMinDwell
	if !b.lastBack.IsZero() && now.Sub(b.lastBack) < fallbackProbation {
		if next = b.dwell * 2; next > fallbackMaxDwell {
			next = fallbackMaxDwell
		}
	}
	if next < fallbackMinDwell {
		next = fallbackMinDwell
	}
	b.on, b.since, b.dwell = true, now, next
	return true
}

// dueBack reports whether the dwell has elapsed and the primary is worth
// re-testing. It is a question about time only — whether REALITY actually
// carries traffic again is a separate, and stronger, piece of evidence the
// caller gathers before acting on this.
func (b *fallbackState) dueBack(now time.Time) bool {
	return b.on && !now.Before(b.since.Add(b.dwell))
}

// leave hands traffic back to the primary and starts probation.
func (b *fallbackState) leave(now time.Time) bool {
	if !b.on {
		return false
	}
	b.on, b.lastBack = false, now
	return true
}

// extend defers the next return attempt by another full dwell, for when the
// return test says the primary still is not carrying.
func (b *fallbackState) extend(now time.Time) {
	if b.on {
		b.since = now
	}
}

// active reports whether traffic is on the fallback leg right now.
func (b *fallbackState) active() bool { return b.on }

// legTestDue reports whether a comparative leg test is warranted right now.
//
// Separated from the test itself, and from the network, for the same reason
// fallbackState is: every one of these conditions was learned from an episode
// that takes a live QoS event to reproduce, and logic that only ever runs in
// production is logic nobody has checked.
func legTestDue(hasReality, onFallback bool, pin string, now, lastTest time.Time) bool {
	if !hasReality || onFallback {
		return false
	}
	// Any pin at all. `mole leg hy2` has already put traffic where this test
	// would send it, and `mole leg reality` is a refusal to go there; in both
	// cases the measurement decides nothing and is only a dial nobody asked for.
	if pin != "" {
		return false
	}
	return now.After(lastTest.Add(legTestInterval))
}

// legTestSaysTransport interprets one comparative leg-test result: an idle leg
// that dials cleanly while new connections on the active leg do not is
// evidence about the active TRANSPORT specifically, which is the only thing
// changing legs can fix. An idle leg that dials just as badly says the
// bottleneck is whatever the two share — the local link, or the route — and
// switching would buy nothing.
//
// slowMs is passed in rather than read from utils so this comparison and the
// fresh-dial latch that triggers it cannot drift apart unnoticed.
func legTestSaysTransport(ms int, err error, slowMs int) bool {
	return err == nil && ms <= slowMs
}

// legAction is what the keepalive streak asks the supervisor to do about the
// leg, computed from facts alone so the policy can be tested without a tunnel.
type legAction int

const (
	legHold legAction = iota
	// legKick rotates the hy2 five-tuple (the stall kick). Only meaningful on
	// the ladder: REALITY has no rung to step onto, and stepping onto one would
	// move traffic across transports for 45s under the name of a kick.
	legKick
	// legFallback hands traffic from the primary to the hy2 ladder.
	legFallback
	// legAbandonFallback hands traffic back from a fallback that is dark too.
	legAbandonFallback
)

// keepaliveLegAction is the escalation policy for a keepalive failure streak.
//
// onHy2 says which transport the streak was measured on; onFallback says
// whether that is because the failover put it there; pin is the `mole leg`
// pin, which disarms every automatic move. probeAlive is the out-of-tunnel UDP
// probe: with it dead the host or the route is gone and no leg is an answer.
func keepaliveLegAction(fails int, probeAlive, onHy2, onFallback bool, pin string) legAction {
	if !probeAlive || fails == 0 {
		return legHold
	}
	if onHy2 && fails == stallKickThreshold {
		return legKick
	}
	if fails < fallbackEnterFails || pin != "" {
		return legHold
	}
	switch {
	case !onHy2:
		return legFallback
	case onFallback:
		return legAbandonFallback
	}
	return legHold
}
