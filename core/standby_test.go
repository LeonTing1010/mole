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
// been seen: the kick is hy2-only because REALITY has no rung to step onto and
// stepping onto one would move traffic across transports under the name of a
// kick; a dead UDP probe means the host or the route is gone and no leg is an
// answer; a pin disarms every move; a fallback that is dark too goes home.
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
		{"kick on hy2 at the threshold", stallKickThreshold, true, true, false, "", legKick},
		{"no kick on REALITY at the threshold", stallKickThreshold, true, false, false, "", legHold},
		{"REALITY dark past the threshold: fall back", fallbackEnterFails, true, false, false, "", legFallback},
		{"fallback dark too: go home", fallbackEnterFails, true, true, true, "", legAbandonFallback},
		{"pinned hy2 dark: hold, the pin is an instruction", fallbackEnterFails, true, true, false, "hy2", legHold},
		{"pinned reality dark: hold", fallbackEnterFails, true, false, false, "reality", legHold},
		{"node without REALITY, hy2 dark: nothing to fall back to", fallbackEnterFails, true, true, false, "", legHold},
		{"below the threshold on REALITY: hold", fallbackEnterFails - 1, true, false, false, "", legHold},
	}
	for _, c := range cases {
		if got := keepaliveLegAction(c.fails, c.probe, c.hy2, c.onFB, c.pin); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
