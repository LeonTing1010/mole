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
	// And the new leg is judged only once it has a full window of its own.
	for i := 0; i < DialWindow-1; i++ {
		s.RecordDial(0, errors.New("timeout"))
	}
	if s.SlowNewFlows {
		t.Fatal("latched on a partial window after reset")
	}
	s.RecordDial(200, nil)
	if !s.SlowNewFlows {
		t.Fatal("a full bad window on the new leg must still latch")
	}
}
