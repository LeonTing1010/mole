package core

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 27, 21, 21, 0, 0, time.UTC)

func TestStandbyEntersOnceAndHoldsMinimumDwell(t *testing.T) {
	var b standbyState
	if !b.enter(t0) {
		t.Fatal("first enter should report a change")
	}
	if b.enter(t0) {
		t.Fatal("entering while already on standby must not report a change")
	}
	if !b.active() {
		t.Fatal("should be on the standby leg")
	}
	if b.dueBack(t0.Add(standbyMinDwell - time.Second)) {
		t.Fatal("returned before the minimum dwell elapsed")
	}
	if !b.dueBack(t0.Add(standbyMinDwell)) {
		t.Fatal("should be due back once the dwell elapses")
	}
}

// A return that survives probation is a real recovery, so the next episode
// starts over at the minimum dwell rather than inheriting a punished one.
func TestStandbyDwellResetsAfterAReturnThatHeld(t *testing.T) {
	var b standbyState
	b.enter(t0)
	back := t0.Add(standbyMinDwell)
	b.leave(back)

	again := back.Add(standbyProbation + time.Minute) // held past probation
	b.enter(again)
	if b.dwell != standbyMinDwell {
		t.Fatalf("dwell = %v, want %v after a return that held", b.dwell, standbyMinDwell)
	}
}

// The failure this guards: a path throttled all evening otherwise costs ~90s of
// dark tunnel once per dwell, forever, because every return looks like the
// first one.
func TestStandbyDwellDoublesWhenTheReturnFailsProbation(t *testing.T) {
	var b standbyState
	b.enter(t0)
	back := t0.Add(standbyMinDwell)
	b.leave(back)

	b.enter(back.Add(time.Minute)) // straight back inside probation
	if want := 2 * standbyMinDwell; b.dwell != want {
		t.Fatalf("dwell = %v, want %v", b.dwell, want)
	}

	back2 := back.Add(time.Minute).Add(b.dwell)
	b.leave(back2)
	b.enter(back2.Add(time.Minute))
	if want := 4 * standbyMinDwell; b.dwell != want {
		t.Fatalf("dwell = %v, want %v", b.dwell, want)
	}
}

func TestStandbyDwellIsCapped(t *testing.T) {
	var b standbyState
	now := t0
	for i := 0; i < 12; i++ {
		b.enter(now)
		now = now.Add(b.dwell)
		b.leave(now)
		now = now.Add(time.Minute) // always inside probation
	}
	b.enter(now)
	if b.dwell > standbyMaxDwell {
		t.Fatalf("dwell = %v, exceeds cap %v", b.dwell, standbyMaxDwell)
	}
	if b.dwell != standbyMaxDwell {
		t.Fatalf("dwell = %v, want it pinned at the cap %v", b.dwell, standbyMaxDwell)
	}
}

// extend is what a failed return test does: hy2 answered nothing, so defer the
// next attempt by a whole dwell instead of retrying every scheduler tick.
func TestStandbyExtendDefersTheNextAttempt(t *testing.T) {
	var b standbyState
	b.enter(t0)
	due := t0.Add(standbyMinDwell)
	if !b.dueBack(due) {
		t.Fatal("precondition: should be due back")
	}
	b.extend(due)
	if b.dueBack(due) {
		t.Fatal("extend must push the next attempt out")
	}
	if !b.dueBack(due.Add(standbyMinDwell)) {
		t.Fatal("should be due back a full dwell after the extension")
	}
}

func TestStandbyLeaveAndExtendAreNoOpsWhenOff(t *testing.T) {
	var b standbyState
	if b.leave(t0) {
		t.Fatal("leaving when not on standby must not report a change")
	}
	b.extend(t0) // must not panic or arm anything
	if b.active() || b.dueBack(t0.Add(time.Hour)) {
		t.Fatal("a state that was never entered must stay inert")
	}
}
