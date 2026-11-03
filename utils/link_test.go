package utils

import "testing"

// fill pushes n echoes with the given RTT and n loses, in total a full window.
func fill(s *State, okMs, okCount, lost int) {
	for i := 0; i < okCount; i++ {
		s.RecordLink(okMs, true)
	}
	for i := 0; i < lost; i++ {
		s.RecordLink(0, false)
	}
}

// The 2026-10-14 false positive: a router that answers in 3ms and drops three
// echoes in a minute is de-prioritising ICMP, not failing to carry traffic.
// Nine such latches cost 40-60s of frozen leg and ceiling decisions each.
func TestLinkLossNeedsCorroboratingRTT(t *testing.T) {
	var s State
	fill(&s, 3, 9, 3)
	if len(s.LinkSamples) != LinkWindow {
		t.Fatalf("window not full: %d", len(s.LinkSamples))
	}
	if s.LinkBad {
		t.Fatalf("3/12 lost with a 3ms median latched a bad link (loss=%d med=%d)", s.LinkLoss(), s.LinkMedianMs())
	}
}

// The same loss with the answered echoes straining is a real verdict.
func TestLinkLossWithSlowAnswersLatches(t *testing.T) {
	var s State
	fill(&s, 150, 9, 3)
	if !s.LinkBad {
		t.Fatalf("3/12 lost with a 150ms median did not latch (med=%d)", s.LinkMedianMs())
	}
}

// The RTT arm still stands on its own.
func TestLinkSlowMedianLatchesWithoutLoss(t *testing.T) {
	var s State
	fill(&s, 250, LinkWindow, 0)
	if !s.LinkBad {
		t.Fatalf("250ms median with no loss did not latch (med=%d)", s.LinkMedianMs())
	}
}

// And the latch still clears once both arms are clean.
func TestLinkLatchClearsWhenBothArmsRecover(t *testing.T) {
	var s State
	fill(&s, 250, LinkWindow, 0)
	if !s.LinkBad {
		t.Fatal("setup: want a latched link")
	}
	s.ResetLink()
	fill(&s, 80, 11, 1)
	if s.LinkBad {
		t.Fatalf("1/12 lost with an 80ms median stayed bad (loss=%d med=%d)", s.LinkLoss(), s.LinkMedianMs())
	}
}
