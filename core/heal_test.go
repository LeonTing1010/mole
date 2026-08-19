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

	if got := h.tick(false); got != 0 {
		t.Fatalf("one bad tick moved the ceiling to backoff %d; it should wait for confirmation", got)
	}
	if got := h.tick(false); got != 1 {
		t.Fatalf("backoff after %d bad ticks = %d, want 1", healBackoffTicks, got)
	}
}

// A single good minute in the middle of a bad stretch is the normal shape of a
// flapping path — it must not be read as recovery, or the ceiling ratchets back
// up into the same failure every couple of minutes.
func TestHealDoesNotClimbOnABriefRecovery(t *testing.T) {
	var h healState
	h.tick(false)
	h.tick(false) // backoff 1

	for i := 0; i < healRecoverTicks-1; i++ {
		if got := h.tick(true); got != 1 {
			t.Fatalf("backoff dropped to %d after only %d clean ticks", got, i+1)
		}
	}
	if got := h.tick(true); got != 0 {
		t.Fatalf("backoff after %d clean ticks = %d, want 0", healRecoverTicks, got)
	}
}

// Alternating good/bad — the exact pattern the logs showed during a flapping
// afternoon — must not oscillate the ceiling. Neither counter should ever reach
// its threshold, so the ceiling stays put.
func TestHealIgnoresFlapping(t *testing.T) {
	var h healState
	for i := 0; i < 40; i++ {
		if got := h.tick(i%2 == 0); got != 0 {
			t.Fatalf("alternating health moved the ceiling to backoff %d at tick %d", got, i)
		}
	}
}

// Descent is bounded. Past a few rungs the ceiling is no longer what is wrong,
// and continuing to throttle only slows the recovery we are waiting for.
func TestHealDescentIsBounded(t *testing.T) {
	var h healState
	got := 0
	for i := 0; i < 200; i++ {
		got = h.tick(false)
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
		h.tick(false)
	}
	for i := 0; i < healMaxBackoff*healRecoverTicks; i++ {
		h.tick(true)
	}
	if h.backoff != 0 {
		t.Fatalf("backoff after a long clean stretch = %d, want 0", h.backoff)
	}
	for i := 0; i < 100; i++ {
		if got := h.tick(true); got != 0 {
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
		h.tick(false)
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
		h.tick(false)
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
		h.tick(false)
	}
	if h.backoff == 0 {
		t.Error("repeated retarget with the same pin wiped the descent; a steady pin must not block self-healing")
	}
}
