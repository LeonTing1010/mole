package core

import (
	"errors"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// The comparative gate is the whole point of wiring the fresh-dial arm to the
// failover, so these cases are the ones that matter. Each was measured, and
// they look identical to the arm that triggers them: "new connections are
// failing". Arguments are (hy2, reality) — the idle leg first, the active one
// second, both timed against the same destination.
func TestLegTestSeparatesUDPFromTheSharedSegment(t *testing.T) {
	// 2026-09-01, 14:46-18:00: UDP specifically. An A/B in the same minute on
	// the same server put REALITY at 0/20 failures against hy2's 3/20, and the
	// healthy leg's dials sat at 440-495ms while the failing one dragged.
	if !legTestSaysTransport(462*time.Millisecond, 2800*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("an idle leg at 462ms against an active leg at 2800ms is a transport fault — switch")
	}
	// 2026-09-01, 18:30: the Wi-Fi PHY rate was flapping 51<->97 Mbps against a
	// co-channel neighbour. Both legs measured the same. Switching would have
	// bought a slower leg and ~260ms more per connection for nothing.
	if legTestSaysTransport(3200*time.Millisecond, 3400*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("an idle leg that is just as slow means the bottleneck is shared — hold")
	}
	// A leg that will not dial at all is the strongest possible version of the
	// same answer.
	if legTestSaysTransport(0, 2800*time.Millisecond, errors.New("context deadline exceeded"), utils.DialSlowMs) {
		t.Error("a leg that will not dial is not evidence for switching to it")
	}
}

// 2026-09-12, 20:01 through 22:25: the gate fired fourteen times in one
// evening and was wrong every time, because the two legs were being measured
// by two different instruments — the active one by the fresh-dial record of
// real user connections, the idle one by a single clash.TestDelay reporting
// 345-752ms. Timed against each other on one destination the same evening,
// REALITY answered in 640-950ms and hy2 in 2300-7200ms.
//
// Both halves of the regression matter: the version that only compared would
// still have switched on a coin-flip, and the version that only thresholded
// would still have switched whenever hy2 cleared 1000ms on its own.
func TestLegTestDoesNotSwitchOntoTheWorseLeg(t *testing.T) {
	if legTestSaysTransport(2300*time.Millisecond, 950*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("hy2 at 2300ms against REALITY's 950ms is the leg to stay off, not move to")
	}
	// The narrower trap: hy2 inside the absolute bar, but no better than the
	// leg already carrying traffic. "Under 1000ms" was never the question.
	if legTestSaysTransport(900*time.Millisecond, 780*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("an idle leg that is merely not-slow is not a reason to commit the dwell to it")
	}
	// And the margin itself: a gap inside the noise of three probes is not a
	// finding. 700 against 1000 is 1.43x, short of the 1.5x the rule demands.
	if legTestSaysTransport(700*time.Millisecond, 1000*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("1.43x is inside the noise — hold and re-test next tick")
	}
	if !legTestSaysTransport(600*time.Millisecond, 1000*time.Millisecond, nil, utils.DialSlowMs) {
		t.Error("1.67x clears the margin — switch")
	}
}

// The absolute bar is a boundary, and the numbers on either side of it came
// from measurement rather than taste: no healthy dial on either leg has ever
// been seen above 1s, and no broken one below 2s. Held against an active leg
// slow enough that the comparison is not what decides these.
func TestLegTestThresholdIsInclusive(t *testing.T) {
	hopeless := 10 * time.Second
	if !legTestSaysTransport(utils.DialSlowMs*time.Millisecond, hopeless, nil, utils.DialSlowMs) {
		t.Error("a dial exactly at the threshold is not yet bad")
	}
	if legTestSaysTransport((utils.DialSlowMs+1)*time.Millisecond, hopeless, nil, utils.DialSlowMs) {
		t.Error("a dial past the threshold is bad however badly the other leg is doing")
	}
}

func TestLegTestDueRequiresAStandbyLegToTest(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	if legTestDue(false, false, utils.LegAuto, time.Now(), long) {
		t.Error("a node with no REALITY leg has nothing to compare against")
	}
	if !legTestDue(true, true, utils.LegAuto, time.Now(), long) {
		t.Error("a failing standby must be able to compare the recovered primary")
	}
	if !legTestDue(true, false, utils.LegAuto, time.Now(), long) {
		t.Error("armed, on hy2, unpinned and long overdue should test")
	}
}

// A pin takes the decision away from the evidence, so the measurement that
// feeds the decision should not be taken either. `mole leg hy2` is a refusal to
// move; `mole leg reality` has already moved. Dialing anyway would put a probe
// on the wire every minute of an episode to inform nobody.
func TestLegTestDueRespectsBothPins(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	for _, pin := range []string{utils.LegHy2, utils.LegReality} {
		if legTestDue(true, false, pin, time.Now(), long) {
			t.Errorf("pin %q should suppress the comparative test", pin)
		}
	}
}

// The fresh-dial latch is a Schmitt trigger that releases at 5%, so it can stay
// set for the length of a whole episode — 98 minutes on 2026-09-01. Without the
// floor, "both legs are equally bad" gets re-measured every 15s for all of them.
func TestLegTestDueRateLimits(t *testing.T) {
	now := time.Now()
	if legTestDue(true, false, utils.LegAuto, now, now.Add(-legTestInterval/2)) {
		t.Error("a test half an interval ago is too recent to repeat")
	}
	if legTestDue(true, false, utils.LegAuto, now, now.Add(-legTestInterval-time.Second)) {
		return
	}
	t.Error("past the interval the test should run again")
}

// The zero value is the first beat after startup: nothing has been measured, so
// nothing should be suppressed.
func TestLegTestDueAllowsTheFirstTest(t *testing.T) {
	if !legTestDue(true, false, utils.LegAuto, time.Now(), time.Time{}) {
		t.Error("the first comparative test must not be rate-limited by a zero timestamp")
	}
}

// The gate to the comparative test. A silent UDP probe must not hold it shut
// when there is a second leg to ask — the outage that taught this had the
// probe silent for its whole length (2026-09-06 22:15–22:19).
func TestLegGateOpensOnASilentProbeWhenThereIsALegToTest(t *testing.T) {
	if !legGateOpen(true, false, true) {
		t.Error("new connections failing, probe silent, REALITY present: the idle leg's dial is the proof — test")
	}
	if legGateOpen(true, false, false) {
		t.Error("single-leg node with a silent probe: nothing to test against, hold")
	}
	if legGateOpen(false, true, true) {
		t.Error("nothing failing: no test")
	}
	if !legGateOpen(true, true, false) {
		t.Error("probe alive still opens the gate on its own")
	}
}
