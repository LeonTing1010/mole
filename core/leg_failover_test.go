package core

import (
	"errors"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// The comparative gate is the whole point of wiring the fresh-dial arm to the
// failover, so these two cases are the ones that matter. Both were measured on
// 2026-09-01, hours apart, and they look identical to the arm that triggers
// them: "new connections are failing".
func TestLegTestSeparatesUDPFromTheSharedSegment(t *testing.T) {
	// 14:46-18:00 that day: UDP specifically. An A/B in the same minute on the
	// same server put REALITY at 0/20 failures against hy2's 3/20, and the
	// standby leg's healthy dials sat at 440-495ms.
	if !legTestSaysTransport(462, nil, utils.DialSlowMs) {
		t.Error("a standby leg dialing at 462ms while new connections fail is UDP-specific — switch")
	}
	// 18:30 that day: the Wi-Fi PHY rate was flapping 51<->97 Mbps against a
	// co-channel neighbour. Both legs measured the same. Switching would have
	// bought a slower leg and ~260ms more per connection for nothing.
	if legTestSaysTransport(3200, nil, utils.DialSlowMs) {
		t.Error("a standby leg that is just as slow means the bottleneck is shared — hold")
	}
	// A standby leg that will not dial at all is the strongest possible version
	// of the same answer.
	if legTestSaysTransport(0, errors.New("context deadline exceeded"), utils.DialSlowMs) {
		t.Error("a standby leg that will not dial is not evidence for switching to it")
	}
}

// The threshold is a boundary, and the numbers on either side of it came from
// measurement rather than taste: no healthy dial on either leg has ever been
// seen above 1s, and no broken one below 2s.
func TestLegTestThresholdIsInclusive(t *testing.T) {
	if !legTestSaysTransport(utils.DialSlowMs, nil, utils.DialSlowMs) {
		t.Error("a dial exactly at the threshold is not yet bad")
	}
	if legTestSaysTransport(utils.DialSlowMs+1, nil, utils.DialSlowMs) {
		t.Error("a dial past the threshold is bad")
	}
}

func TestLegTestDueRequiresAStandbyLegToTest(t *testing.T) {
	long := time.Now().Add(-time.Hour)
	if legTestDue(false, false, utils.LegAuto, time.Now(), long) {
		t.Error("a node with no REALITY leg has nothing to compare against")
	}
	if legTestDue(true, true, utils.LegAuto, time.Now(), long) {
		t.Error("already on the standby leg — the question is answered")
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
