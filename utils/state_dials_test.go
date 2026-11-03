package utils

import (
	"errors"
	"testing"
)

// A leg change must not carry the previous leg's fresh-dial verdict across.
// 2026-09-05: fifteen hy2 samples convicted REALITY 72 seconds after it was
// handed traffic back.
func TestResetDialsEmptiesWindowAndLatch(t *testing.T) {
	var s State
	for i := 0; i < DialWindow; i++ {
		s.RecordDial(0, errors.New("timeout"))
	}
	if !s.SlowNewFlows || s.DialBad() != DialWindow {
		t.Fatalf("setup: want a latched, fully bad window; got latch=%v bad=%d", s.SlowNewFlows, s.DialBad())
	}
	s.ResetDials()
	if s.SlowNewFlows || len(s.DialSamples) != 0 {
		t.Fatalf("after reset: latch=%v samples=%d, want false/0", s.SlowNewFlows, len(s.DialSamples))
	}
	// The new leg is judged on its own samples, without a full-window delay.
	for i := 0; i < DialBadEnter-1; i++ {
		s.RecordDial(0, errors.New("timeout"))
	}
	if s.SlowNewFlows {
		t.Fatal("latched before enough evidence on the new leg")
	}
	s.RecordDial(0, errors.New("timeout"))
	if !s.SlowNewFlows {
		t.Fatal("four failures on the new leg must latch")
	}

}

// The distinction that stopped the 2026-10-14 ping-pong. DialBad counts a slow
// dial as bad — that is its job, and it is what marks the leg degraded in
// `mole status` — but DialFails counts only the dials that carried NOTHING.
// That evening all six leg switches were opened by four SUCCESSFUL 1.1-2.4s
// dials: DialBad called them bad and moved the tunnel; DialFails calls none of
// them a failure, which is the correct reading of a connection that answered.
func TestDialFailsIgnoresSlowButSuccessfulDials(t *testing.T) {
	var s State
	for i := 0; i < DialWindow; i++ {
		s.RecordDial(DialSlowMs+500, nil) // answered, just slowly
	}
	if got := s.DialBad(); got != DialWindow {
		t.Fatalf("setup: DialBad = %d, want %d — a slow dial is a bad dial", got, DialWindow)
	}
	if got := s.DialFails(); got != 0 {
		t.Fatalf("DialFails = %d, want 0 — a dial that answered is not a failure", got)
	}
	for i := 0; i < DialBadEnter; i++ {
		s.RecordDial(0, errors.New("timeout"))
	}
	if got := s.DialFails(); got != DialBadEnter {
		t.Fatalf("DialFails = %d, want %d after %d timeouts", got, DialBadEnter, DialBadEnter)
	}
}

// Quality/what: a broken new leg must be detected before the full window fills.
func TestFreshDialFailuresTriggerEarly(t *testing.T) {
	// Why: waiting for twenty samples leaves new pages failing for minutes.
	for _, ms := range []int{0, DialSlowMs + 1} {
		var s State
		for i := 0; i < DialBadEnter; i++ {
			if ms == 0 {
				s.RecordDial(0, errors.New("timeout"))
			} else {
				s.RecordDial(ms, nil)
			}
			if i < DialBadEnter-1 && s.SlowNewFlows {
				t.Fatal("one brief hiccup triggered failover")
			}
		}
		if !s.SlowNewFlows || s.Health() != HealthDegraded {
			t.Fatal("four bad samples must mark the new leg degraded")
		}
	}
}
