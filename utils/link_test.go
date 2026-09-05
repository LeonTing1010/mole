package utils

import "testing"

// fill slides a whole window of samples in without touching the latch, the way
// the probe does.
func fill(s *State, lost int, ms int) {
	for i := 0; i < LinkWindow; i++ {
		if i < lost {
			s.RecordLink(0, false)
		} else {
			s.RecordLink(ms, true)
		}
	}
}

// The latch enters on loss or on delay, and does not decide on a partial window.
func TestLinkLatchEntersOnLossOrDelay(t *testing.T) {
	var s State
	for i := 0; i < LinkWindow-1; i++ {
		s.RecordLink(0, false)
	}
	if s.LinkBad {
		t.Fatal("latched on a partial window")
	}
	fill(&s, LinkBadLoss, 10)
	if !s.LinkBad {
		t.Fatalf("loss %d/%d did not latch", LinkBadLoss, LinkWindow)
	}
	fill(&s, 0, LinkBadMedianMs)
	if !s.LinkBad {
		t.Fatalf("median %dms did not latch", LinkBadMedianMs)
	}
}

// Leaving needs BOTH arms clean; the band between the thresholds holds.
func TestLinkLatchHysteresis(t *testing.T) {
	var s State
	fill(&s, LinkBadLoss, 10)
	if !s.LinkBad {
		t.Fatal("setup")
	}
	fill(&s, LinkOKLoss+1, 10) // 2 lost: below enter, above leave
	if !s.LinkBad {
		t.Fatal("released inside the hysteresis band")
	}
	fill(&s, LinkOKLoss, 10)
	if s.LinkBad {
		t.Fatal("did not release on a clean window")
	}
	// A latched link with a full window whose RTT sits exactly at the leave
	// bound stays latched: leaving needs strictly below. Built fresh so no
	// intermediate (clean) window slides through on the way.
	var b State
	b.LinkBad = true
	fill(&b, LinkOKLoss, LinkOKMedianMs)
	if !b.LinkBad {
		t.Fatal("released with median at the OK bound (must be below)")
	}
}

// One 1s outlier in a clean minute is not a bad link.
func TestLinkLatchIgnoresSingleOutlier(t *testing.T) {
	var s State
	for i := 0; i < LinkWindow; i++ {
		ms := 5
		if i == 3 {
			ms = 1000
		}
		s.RecordLink(ms, true)
	}
	if s.LinkBad {
		t.Fatal("a single outlier latched the link bad")
	}
}
