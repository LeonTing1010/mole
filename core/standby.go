package core

import "time"

// RealityStandbyTag names the selector member carrying the VLESS+REALITY leg.
//
// It lives in the SAME selector as the Brutal ladder rungs, which is the whole
// trick: moving traffic between two different transport protocols costs exactly
// what a ceiling change costs — one Clash API call, no sing-box restart, no
// rebuilt TUN, no dropped connections. See buildProxyOutbounds.
const RealityStandbyTag = "proxy-reality"

const (
	// standbyEnterFails is how many consecutive in-tunnel keepalive failures —
	// with the out-of-tunnel probe still answering — hand traffic to REALITY.
	//
	// At the 15s keepalive cadence that is ~90s, deliberately AFTER the stall
	// kick has had its turn (it fires at 2 failures and holds for 45s). The kick
	// is the cheaper answer and sometimes the right one: it stays on UDP, where
	// Brutal is, and it costs one re-dial. Escalating past it means the cheap
	// answer was tried and did not take.
	//
	// Why it must exist at all, measured 2026-08-27 21:21–21:27 CST: the kick
	// redialled four times and the server logged four sessions dying after
	// exactly 30.0s — hysteria's idle timeout — each from a different source
	// port. Rotating the source port answers nothing when the block keys on
	// something coarser than the port. Meanwhile the direct probe, single
	// datagrams to that same ip:port, kept getting answered throughout. A new
	// flow could be established and an established flow could not be carried,
	// which is a statement about UDP on this path and not about the ceiling,
	// the host, or the route. TCP is the only lever left.
	standbyEnterFails = 6

	// standbyMinDwell is how long traffic stays on REALITY before hy2 is even
	// reconsidered. These episodes last minutes, not seconds; returning early
	// just spends another 90s of dark tunnel discovering that.
	standbyMinDwell = 10 * time.Minute
	// standbyMaxDwell caps the doubling below, so a bad night cannot strand the
	// tunnel on the slower leg indefinitely once the path recovers.
	standbyMaxDwell = 60 * time.Minute
	// standbyProbation is how long a return has to survive to count as real. Go
	// back to standby sooner than this and the return was premature, which
	// doubles the next dwell.
	standbyProbation = 3 * time.Minute
)

// standbyState folds the failover decision into a value with no dependencies,
// so the policy can be exercised directly. Same reasoning as healState: the
// condition it guards against takes a live QoS episode to reproduce, and that
// is exactly the kind of logic that otherwise only ever runs in production.
//
// The clock arrives as a parameter rather than being read here, which is what
// makes a ten-minute dwell testable in microseconds.
type standbyState struct {
	on    bool
	since time.Time
	// lastBack is when traffic was last handed back to hy2, used to tell a
	// return that held from one that did not.
	lastBack time.Time
	// dwell is how long THIS stay on REALITY lasts before a return is tried.
	dwell time.Duration
}

// enter moves traffic to the standby leg, and reports whether this call is what
// changed it.
//
// The dwell doubles when the previous return failed probation. Without that, a
// path that is being throttled all evening pays the full detection cost — ~90s
// of dark tunnel — once per dwell, all evening. With it, mole notices the
// pattern and stops re-testing a path that keeps saying no.
func (b *standbyState) enter(now time.Time) bool {
	if b.on {
		return false
	}
	next := standbyMinDwell
	if !b.lastBack.IsZero() && now.Sub(b.lastBack) < standbyProbation {
		if next = b.dwell * 2; next > standbyMaxDwell {
			next = standbyMaxDwell
		}
	}
	if next < standbyMinDwell {
		next = standbyMinDwell
	}
	b.on, b.since, b.dwell = true, now, next
	return true
}

// dueBack reports whether the dwell has elapsed and hy2 is worth re-testing.
// It is a question about time only — whether the UDP leg actually carries
// traffic again is a separate, and stronger, piece of evidence the caller
// gathers before acting on this.
func (b *standbyState) dueBack(now time.Time) bool {
	return b.on && !now.Before(b.since.Add(b.dwell))
}

// leave hands traffic back to the hy2 ladder and starts probation.
func (b *standbyState) leave(now time.Time) bool {
	if !b.on {
		return false
	}
	b.on, b.lastBack = false, now
	return true
}

// extend defers the next return attempt by another full dwell, for when the
// return test says hy2 still is not carrying.
func (b *standbyState) extend(now time.Time) {
	if b.on {
		b.since = now
	}
}

// active reports whether traffic is on the standby leg right now.
func (b *standbyState) active() bool { return b.on }
