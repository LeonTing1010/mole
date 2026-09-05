package core

import "testing"

// The ladder is the only thing self-healing can walk, so stepping has to stay
// on it: a target that is not itself a rung must snap down to one rather than
// naming an outbound sing-box was never given.
func TestStepDownStaysOnTheLadder(t *testing.T) {
	// Rungs: 1, 2, 3, 5, 8, 12, 20
	s := BandwidthSchedule{OffUp: 5, OffDown: 20, PeakUp: 2, PeakDown: 5, StartHour: 12, EndHour: 2}

	cases := []struct{ down, steps, want int }{
		{20, 1, 12},
		{20, 3, 5},
		{5, 1, 3},
		{3, 2, 1},
		{1, 1, 1},   // already at the bottom
		{20, 99, 1}, // clamps rather than falling off
		{7, 1, 3},   // 7 is not a rung: snap to 5, then step to 3
		{7, 0, 5},   // snapping happens even with no step
	}
	for _, c := range cases {
		if got := s.StepDown(c.down, c.steps); got != c.want {
			t.Errorf("StepDown(%d, %d) = %d, want %d", c.down, c.steps, got, c.want)
		}
		if got := s.StepDown(c.down, c.steps); !s.HasRung(got) {
			t.Errorf("StepDown(%d, %d) = %d, which is not a rung the config carries", c.down, c.steps, got)
		}
	}
}

// A schedule with no Brutal ceiling (BBR mode) has no ladder at all. Stepping
// must report that rather than inventing rung 0, which would select a member
// that does not exist.
func TestStepDownWithNoLadder(t *testing.T) {
	if got := (BandwidthSchedule{}).StepDown(20, 1); got != 0 {
		t.Errorf("StepDown with no ladder = %d, want 0", got)
	}
}

func TestHealDescendsOnlyAfterSustainedTrouble(t *testing.T) {
	var h healState

	if got := h.tick(false, true, false); got != 0 {
		t.Fatalf("one bad tick moved the ceiling to backoff %d; it should wait for confirmation", got)
	}
	if got := h.tick(false, true, false); got != 1 {
		t.Fatalf("backoff after %d bad ticks = %d, want 1", healBackoffTicks, got)
	}
}

// A single good minute in the middle of a bad stretch is the normal shape of a
// flapping path — it must not be read as recovery, or the ceiling ratchets back
// up into the same failure every couple of minutes.
func TestHealDoesNotClimbOnABriefRecovery(t *testing.T) {
	var h healState
	h.tick(false, true, false)
	h.tick(false, true, false) // backoff 1

	for i := 0; i < healRecoverTicks-1; i++ {
		if got := h.tick(true, false, false); got != 1 {
			t.Fatalf("backoff dropped to %d after only %d clean ticks", got, i+1)
		}
	}
	if got := h.tick(true, false, false); got != 0 {
		t.Fatalf("backoff after %d clean ticks = %d, want 0", healRecoverTicks, got)
	}
}

// Alternating good/bad — the exact pattern the logs showed during a flapping
// afternoon — must not oscillate the ceiling. Neither counter should ever reach
// its threshold, so the ceiling stays put.
//
// The FLOOD signal is what alternates here. This test used to hold flooding
// true on every tick and alternate only health, which made it assert something
// nobody wanted: that a sustained flood should never lower the ceiling as long
// as the health verdict kept flapping. That is exactly the hole found on
// 2026-08-22 — 45 ENOBUFS in 45 minutes against a ceiling the Wi-Fi could not
// carry, and not one descent, because the keepalive kept recovering between
// failures. Flooding is now decisive on its own (see healState.tick), so the
// anti-oscillation property has to be stated in terms of the evidence that
// actually moves the ceiling.
func TestHealIgnoresFlapping(t *testing.T) {
	var h healState
	for i := 0; i < 40; i++ {
		if got := h.tick(true, i%2 == 0, false); got != 0 {
			t.Fatalf("alternating flood evidence moved the ceiling to backoff %d at tick %d", got, i)
		}
	}
}

// Descent is bounded. Past a few rungs the ceiling is no longer what is wrong,
// and continuing to throttle only slows the recovery we are waiting for.
func TestHealDescentIsBounded(t *testing.T) {
	var h healState
	got := 0
	for i := 0; i < 200; i++ {
		got = h.tick(false, true, false)
	}
	if got != healMaxBackoff {
		t.Errorf("backoff under continuous failure = %d, want it capped at %d", got, healMaxBackoff)
	}
}

// Recovery must unwind every rung it took, one long clean stretch at a time,
// and then stop at the operator's target rather than climbing past it.
func TestHealReturnsToTheTargetAndStops(t *testing.T) {
	var h healState
	for i := 0; i < 200; i++ {
		h.tick(false, true, false)
	}
	for i := 0; i < healMaxBackoff*healRecoverTicks; i++ {
		h.tick(true, false, false)
	}
	if h.backoff != 0 {
		t.Fatalf("backoff after a long clean stretch = %d, want 0", h.backoff)
	}
	for i := 0; i < 100; i++ {
		if got := h.tick(true, false, false); got != 0 {
			t.Fatalf("backoff went negative (%d) — the ceiling climbed above its target", got)
		}
	}
}

// The bug this guards: `mole ceiling 30` reported 30, the Clash selector really
// was proxy-bw-30, and `mole status` said "pinned↓2 (Brutal ↓5 Mbps)" — the
// backoff earned against the *previous* ceiling was still being subtracted from
// the new one. A pin is a fresh statement of intent; the descent has to re-earn
// itself from the probes rather than carrying over.
func TestHealBackoffResetsWhenTheCeilingTargetChanges(t *testing.T) {
	h := healState{target: 30}
	for i := 0; i < healBackoffTicks*2; i++ {
		h.tick(false, true, false)
	}
	if h.backoff == 0 {
		t.Fatal("setup failed: the path was unhealthy long enough to descend, but backoff is 0")
	}

	h.retarget(12)
	if h.backoff != 0 {
		t.Errorf("backoff after pinning a new ceiling = %d, want 0 — the user's number must be what runs", h.backoff)
	}
	if h.target != 12 {
		t.Errorf("target after retarget = %d, want 12", h.target)
	}
	// The partial evidence has to go too, or the next bad tick descends
	// immediately off a count gathered against the old ceiling.
	if h.bad != 0 || h.good != 0 {
		t.Errorf("retarget left bad=%d good=%d, want both 0", h.bad, h.good)
	}
}

// Retiring a pin (back to clock control, pin 0) is the same kind of change, and
// the same tick must not keep descending below the clock's own target.
func TestHealBackoffResetsWhenThePinIsRetired(t *testing.T) {
	h := healState{target: 30}
	for i := 0; i < healBackoffTicks*2; i++ {
		h.tick(false, true, false)
	}
	h.retarget(0)
	if h.backoff != 0 {
		t.Errorf("backoff after `mole ceiling auto` = %d, want 0", h.backoff)
	}
}

// Every tick calls retarget with the pin it just read, so an unchanged pin must
// be inert — otherwise the descent could never accumulate at all.
func TestHealRetargetIsInertWhenTheTargetIsUnchanged(t *testing.T) {
	h := healState{target: 30}
	for i := 0; i < healBackoffTicks*2; i++ {
		h.retarget(30)
		h.tick(false, true, false)
	}
	if h.backoff == 0 {
		t.Error("repeated retarget with the same pin wiped the descent; a steady pin must not block self-healing")
	}
}

// The regression this exists for: on 2026-08-22 the tunnel went dark 25 times
// and self-healing lowered the ceiling for 24 of them. Only one of those events
// carried any ENOBUFS — the other 24 were a dark path, which no rung on the
// ladder can fix. Each descent cost half the bandwidth for at least
// healRecoverTicks minutes and bought nothing.
func TestHealHoldsTheCeilingOnADarkPath(t *testing.T) {
	var h healState
	for i := 0; i < 200; i++ {
		if got := h.tick(false, false, false); got != 0 {
			t.Fatalf("a dark path with no flooding descended to backoff %d at tick %d; the ceiling is not what is wrong", got, i)
		}
	}
}

// Holding is not the same as calling the path healthy. A dark stretch must not
// count toward the clean streak either, or a ceiling that really was too high
// climbs straight back up while the link is still down.
func TestHealDoesNotClimbWhileThePathIsDark(t *testing.T) {
	var h healState
	h.tick(false, true, false)
	h.tick(false, true, false) // backoff 1, earned against real flooding
	if h.backoff != 1 {
		t.Fatalf("setup failed: backoff = %d, want 1", h.backoff)
	}
	for i := 0; i < healRecoverTicks*3; i++ {
		if got := h.tick(false, false, false); got != 1 {
			t.Fatalf("dark ticks moved the ceiling to backoff %d at tick %d; they must hold it", got, i)
		}
	}
}

// One burst of ENOBUFS inside a single tick is not evidence of a ceiling that is
// too high — a blackhole produces some as the buffer backs up too. Descending
// still needs healBackoffTicks worth of it.
func TestHealDoesNotDescendOnASingleFloodingTick(t *testing.T) {
	var h healState
	h.tick(false, true, false)
	for i := 0; i < 50; i++ {
		if got := h.tick(false, false, false); got != 0 {
			t.Fatalf("one flooding tick descended to backoff %d; it takes %d", got, healBackoffTicks)
		}
	}
}

// Evidence accumulates across a gap rather than resetting on it: flooding that
// keeps recurring through a bad stretch is a ceiling problem even when
// individual minutes come back quiet.
func TestHealDescendsOnFloodingSpreadAcrossABadStretch(t *testing.T) {
	var h healState
	h.tick(false, true, false)
	h.tick(false, false, false)
	if got := h.tick(false, true, false); got != 1 {
		t.Errorf("backoff after two flooding ticks either side of a dark one = %d, want 1", got)
	}
}

// A clean minute is the one thing that does clear the evidence — the ceiling
// proved itself at that rung, so the next descent starts from scratch.
func TestHealFloodEvidenceIsClearedByARecovery(t *testing.T) {
	var h healState
	h.tick(false, true, false)
	h.tick(true, false, false)
	if got := h.tick(false, true, false); got != 0 {
		t.Errorf("backoff = %d after a healthy minute reset the count, want 0", got)
	}
}

// The stall kick needs a rung that is NOT the one it is leaving — the whole
// point is to route through a different outbound so a fresh QUIC session gets
// dialed. Returning the same rung would select the stuck session again.
func TestSiblingAlwaysMoves(t *testing.T) {
	// Rungs: 1, 2, 3, 5, 8, 12, 20
	s := BandwidthSchedule{OffUp: 5, OffDown: 20, PeakUp: 2, PeakDown: 5, StartHour: 12, EndHour: 2}

	for _, down := range s.Rungs() {
		got := s.Sibling(down)
		if got == 0 {
			t.Errorf("Sibling(%d) = 0, want a rung to step onto", down)
			continue
		}
		if got == down {
			t.Errorf("Sibling(%d) = %d — the kick would re-select the stuck session", down, got)
		}
		if !s.HasRung(got) {
			t.Errorf("Sibling(%d) = %d, which is not a rung the config carries", down, got)
		}
	}
	// Prefers stepping down: momentarily declaring MORE bandwidth than the rung
	// that was already struggling is the wrong direction.
	if got := s.Sibling(20); got != 12 {
		t.Errorf("Sibling(20) = %d, want 12 (one rung down)", got)
	}
	// ...except at the bottom, where down is not available.
	if got := s.Sibling(1); got != 2 {
		t.Errorf("Sibling(1) = %d, want 2 (nothing below to step to)", got)
	}
}

// With no ladder there is nothing to kick onto, and saying so is what keeps the
// supervisor from selecting a member sing-box never defined.
func TestSiblingWithNoLadder(t *testing.T) {
	if got := (BandwidthSchedule{}).Sibling(20); got != 0 {
		t.Errorf("Sibling with no ladder = %d, want 0", got)
	}
}

// A local uplink outage carries no evidence about the ceiling, so it must not
// consume the clean streak. This is the regression that made a 60-second Wi-Fi
// re-association cost fifteen extra minutes at half bandwidth.
func TestFreezePreservesTheCleanStreak(t *testing.T) {
	var h healState
	h.target = 20

	// Earn a backoff the honest way: unhealthy AND flooding.
	for i := 0; i < healBackoffTicks; i++ {
		h.tick(false, true, false)
	}
	if h.backoff != 1 {
		t.Fatalf("setup: backoff = %d, want 1", h.backoff)
	}

	// Bank most of the clean run needed to climb back.
	for i := 0; i < healRecoverTicks-1; i++ {
		h.tick(true, false, false)
	}
	banked := h.good

	// The uplink drops for a few ticks. Nothing is known about the path.
	for i := 0; i < 3; i++ {
		if got := h.freeze(); got != 1 {
			t.Errorf("freeze() = %d, want the backoff left untouched (1)", got)
		}
	}
	if h.good != banked {
		t.Errorf("clean streak = %d after freezing, want it preserved at %d", h.good, banked)
	}

	// One more good tick and the ceiling climbs — the frozen ticks neither
	// helped nor hurt.
	if got := h.tick(true, false, false); got != 0 {
		t.Errorf("tick after the link returned = %d, want 0 (climbed a rung)", got)
	}
}

// Degraded means the tunnel is still carrying traffic, so it must not be fed to
// tick() as unhealthy — that clears the streak, and the probe loses three in a
// row often enough that the ceiling could never climb back.
func TestDegradedTicksAsUsable(t *testing.T) {
	var h healState
	h.target = 20
	for i := 0; i < healBackoffTicks; i++ {
		h.tick(false, true, false)
	}

	// utils.HealthDegraded.Usable() is true, so this is what retune passes.
	for i := 0; i < healRecoverTicks; i++ {
		h.tick(true, false, false)
	}
	if h.backoff != 0 {
		t.Errorf("backoff = %d after %d usable ticks, want 0", h.backoff, healRecoverTicks)
	}
}

// Flood evidence must survive a flapping health verdict. The keepalive recovers
// between failures, so a bad stretch reads "healthy" on most ticks; when the
// descent was gated on unhealthy AND flooding, those healthy ticks reset the
// counter and the ceiling never came down. Measured 2026-08-22: a link whose
// Wi-Fi rate had collapsed to 14-47 Mbps against a 65 Mbps ceiling produced 45
// ENOBUFS in 45 minutes and not one descent.
func TestFloodingDescendsThroughFlappingHealth(t *testing.T) {
	var h healState
	h.target = 65

	// Alternate healthy/unhealthy while flooding throughout — the exact shape
	// that used to produce no descent at all.
	for i := 0; i < healBackoffTicks*2; i++ {
		h.tick(i%2 == 0, true, false)
	}
	if h.backoff == 0 {
		t.Fatal("flooding through a flapping health verdict produced no descent")
	}
}

// ...but flooding must still be REQUIRED. An unhealthy path with no ENOBUFS is
// a dark path, and descending there buys nothing while costing half the
// bandwidth for healRecoverTicks minutes afterwards. This is the regression
// 074274f fixed and it must not come back.
func TestUnhealthyWithoutFloodingStillHolds(t *testing.T) {
	var h healState
	h.target = 65
	for i := 0; i < healRecoverTicks*3; i++ {
		if got := h.tick(false, false, false); got != 0 {
			t.Fatalf("dark path descended to backoff %d — no rung passes traffic through a blackhole", got)
		}
	}
}

// A healthy, non-flooding link must still climb back, and flooding must reset
// the clean streak that climbing depends on.
func TestFloodingResetsTheClimb(t *testing.T) {
	var h healState
	h.target = 65
	for i := 0; i < healBackoffTicks; i++ {
		h.tick(true, true, false)
	}
	if h.backoff != 1 {
		t.Fatalf("setup: backoff = %d, want 1", h.backoff)
	}
	for i := 0; i < healRecoverTicks-1; i++ {
		h.tick(true, false, false)
	}
	h.tick(true, true, false) // one flood mid-recovery
	if h.good != 0 {
		t.Errorf("clean streak = %d after a flood, want it reset", h.good)
	}
	for i := 0; i < healRecoverTicks-1; i++ {
		h.tick(true, false, false)
	}
	if h.backoff != 1 {
		t.Errorf("backoff = %d — the climb must restart from the flood, not resume", h.backoff)
	}
	h.tick(true, false, false)
	if h.backoff != 0 {
		t.Errorf("backoff = %d after a full clean run, want 0", h.backoff)
	}
}

// ENOBUFS inside a keepalive blackout holds the ceiling; the same flood with
// the tunnel answering still descends. 2026-09-03..05: eighteen bursts, all
// inside blackouts, nine descents, none of them useful.
func TestHealHoldsFloodDuringBlackout(t *testing.T) {
	var h healState
	for i := 0; i < 20; i++ {
		if got := h.tick(false, true, true); got != 0 {
			t.Fatalf("flood during blackout descended to backoff %d at tick %d", got, i)
		}
	}
	// The blackout ends, the flood does not: now it is the ceiling.
	h.tick(true, true, false)
	if got := h.tick(true, true, false); got != 1 {
		t.Fatalf("flood with the tunnel answering did not descend: backoff %d", got)
	}
}
