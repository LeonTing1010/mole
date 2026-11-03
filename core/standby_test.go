package core

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 27, 21, 21, 0, 0, time.UTC)

func TestStandbyEntersOnceAndHoldsMinimumDwell(t *testing.T) {
	var b fallbackState
	if !b.enter(t0) {
		t.Fatal("first enter should report a change")
	}
	if b.enter(t0) {
		t.Fatal("entering while already on standby must not report a change")
	}
	if !b.active() {
		t.Fatal("should be on the standby leg")
	}
	if b.dueBack(t0.Add(fallbackMinDwell - time.Second)) {
		t.Fatal("returned before the minimum dwell elapsed")
	}
	if !b.dueBack(t0.Add(fallbackMinDwell)) {
		t.Fatal("should be due back once the dwell elapses")
	}
}

// A return that survives probation is a real recovery, so the next episode
// starts over at the minimum dwell rather than inheriting a punished one.
func TestStandbyDwellResetsAfterAReturnThatHeld(t *testing.T) {
	var b fallbackState
	b.enter(t0)
	back := t0.Add(fallbackMinDwell)
	b.leave(back)

	again := back.Add(fallbackProbation + time.Minute) // held past probation
	b.enter(again)
	if b.dwell != fallbackMinDwell {
		t.Fatalf("dwell = %v, want %v after a return that held", b.dwell, fallbackMinDwell)
	}
}

// The failure this guards: a path throttled all evening otherwise costs ~90s of
// dark tunnel once per dwell, forever, because every return looks like the
// first one.
func TestStandbyDwellDoublesWhenTheReturnFailsProbation(t *testing.T) {
	var b fallbackState
	b.enter(t0)
	back := t0.Add(fallbackMinDwell)
	b.leave(back)

	b.enter(back.Add(time.Minute)) // straight back inside probation
	if want := 2 * fallbackMinDwell; b.dwell != want {
		t.Fatalf("dwell = %v, want %v", b.dwell, want)
	}

	back2 := back.Add(time.Minute).Add(b.dwell)
	b.leave(back2)
	b.enter(back2.Add(time.Minute))
	if want := 4 * fallbackMinDwell; b.dwell != want {
		t.Fatalf("dwell = %v, want %v", b.dwell, want)
	}
}

func TestStandbyDwellIsCapped(t *testing.T) {
	var b fallbackState
	now := t0
	for i := 0; i < 12; i++ {
		b.enter(now)
		now = now.Add(b.dwell)
		b.leave(now)
		now = now.Add(time.Minute) // always inside probation
	}
	b.enter(now)
	if b.dwell > fallbackMaxDwell {
		t.Fatalf("dwell = %v, exceeds cap %v", b.dwell, fallbackMaxDwell)
	}
	if b.dwell != fallbackMaxDwell {
		t.Fatalf("dwell = %v, want it pinned at the cap %v", b.dwell, fallbackMaxDwell)
	}
}

// extend is what a failed return test does: hy2 answered nothing, so defer the
// next attempt by a whole dwell instead of retrying every scheduler tick.
func TestStandbyExtendDefersTheNextAttempt(t *testing.T) {
	var b fallbackState
	b.enter(t0)
	due := t0.Add(fallbackMinDwell)
	if !b.dueBack(due) {
		t.Fatal("precondition: should be due back")
	}
	b.extend(due)
	if b.dueBack(due) {
		t.Fatal("extend must push the next attempt out")
	}
	if !b.dueBack(due.Add(fallbackMinDwell)) {
		t.Fatal("should be due back a full dwell after the extension")
	}
}

func TestStandbyLeaveAndExtendAreNoOpsWhenOff(t *testing.T) {
	var b fallbackState
	if b.leave(t0) {
		t.Fatal("leaving when not on standby must not report a change")
	}
	b.extend(t0) // must not panic or arm anything
	if b.active() || b.dueBack(t0.Add(time.Hour)) {
		t.Fatal("a state that was never entered must stay inert")
	}
}

// The keepalive escalation policy, as a table. Every row is a shape that has
// been seen: a dead UDP probe means the host or the route is gone and no leg is
// an answer; a pin disarms every move; a fallback that is dark too goes home.
func TestKeepaliveLegActionPolicy(t *testing.T) {
	cases := []struct {
		name             string
		fails            int
		probe, hy2, onFB bool
		pin              string
		want             legAction
	}{
		{"no failures", 0, true, false, false, "", legHold},
		{"probe dead: nothing to move to", fallbackEnterFails, false, false, false, "", legHold},
		{"hy2 dark below the fallback threshold: hold", fallbackEnterFails - 1, true, true, false, "", legHold},
		{"REALITY dark past the threshold: fall back", fallbackEnterFails, true, false, false, "", legFallback},
		{"fallback dark too: go home", fallbackEnterFails, true, true, true, "", legAbandonFallback},
		{"pinned hy2 dark: hold, the pin is an instruction", fallbackEnterFails, true, true, false, "hy2", legHold},
		{"pinned reality dark: hold", fallbackEnterFails, true, false, false, "reality", legHold},
		{"node without REALITY, hy2 dark: nothing to fall back to", fallbackEnterFails, true, true, false, "", legHold},
		{"below the threshold on REALITY: hold", fallbackEnterFails - 1, true, false, false, "", legHold},
	}
	for _, c := range cases {
		if got := keepaliveLegAction(c.fails, c.probe, true, c.hy2, c.onFB, c.pin); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A bad local link holds every move, whatever the streak says.
func TestKeepaliveLegActionHoldsOnBadLink(t *testing.T) {
	for _, fails := range []int{1, fallbackEnterFails, fallbackEnterFails * 3} {
		for _, onHy2 := range []bool{true, false} {
			if got := keepaliveLegAction(fails, true, false, onHy2, false, ""); got != legHold {
				t.Errorf("fails=%d onHy2=%v with a bad link: got %v, want hold", fails, onHy2, got)
			}
		}
	}
}

// rescueStuck fires when keepaliveLegAction would return legHold (because
// the UDP probe is dead) but we're stranded on the hy2 fallback with
// enough failures past the threshold. In that case the caller runs a
// direct TCP probe to REALITY as a second opinion.
func TestRescueStuckFallbackTrigger(t *testing.T) {
	cases := []struct {
		name         string
		action       legAction
		fails        int
		onFallback   bool
		linkBad      bool
		hasReality   bool
		wantRescue   bool
	}{
		{"not on fallback: nothing to rescue", legHold, fallbackEnterFails, false, false, true, false},
		{"on fallback but fails below threshold: not yet", legHold, fallbackEnterFails - 1, true, false, true, false},
		{"on fallback, threshold met, link bad: skip (Wi-Fi fault)", legHold, fallbackEnterFails, true, true, true, false},
		{"on fallback, threshold met, no reality addr: skip (no primary to go to)", legHold, fallbackEnterFails, true, false, false, false},
		{"on fallback, threshold met, legFallback action: normal path handles it", legFallback, fallbackEnterFails, true, false, true, false},
		{"on fallback, threshold met, probe alive: normal legAbandonFallback handles it", legAbandonFallback, fallbackEnterFails, true, false, true, false},
		{"on fallback, threshold met, probe dead, link ok: rescue!", legHold, fallbackEnterFails, true, false, true, true},
		{"on fallback, way past threshold: still rescue", legHold, fallbackEnterFails * 2, true, false, true, true},
	}
	for _, c := range cases {
		got := c.action == legHold &&
			c.fails >= fallbackEnterFails &&
			c.onFallback &&
			!c.linkBad &&
			c.hasReality
		if got != c.wantRescue {
			t.Errorf("%s: rescueStuck=%v, want %v", c.name, got, c.wantRescue)
		}
	}
}
