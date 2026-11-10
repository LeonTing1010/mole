package core

import "testing"

func TestRateGovernorCollapsesWhenDeclarationIsFarAbovePath(t *testing.T) {
	// The case that motivated the governor: 600 Mbps declared on a Wi-Fi link
	// that delivers ~30. It must converge in a few samples, never below the
	// measured rate.
	g := NewRateGovernor(600, 3, 600)

	r, changed, _ := g.Observe(5)
	if !changed || r != 150 {
		t.Fatalf("first sample: got %d changed=%v, want 150 true", r, changed)
	}
	r, changed, _ = g.Observe(25)
	if !changed || r != 37 {
		t.Fatalf("second sample: got %d changed=%v, want 37 true", r, changed)
	}
	// One mild shortfall is not enough — jitter produces single bad samples.
	if r, changed, _ = g.Observe(30); changed {
		t.Fatalf("single mild shortfall moved rate to %d, want no change", r)
	}
	if r, changed, _ = g.Observe(30); !changed || r != 31 {
		t.Fatalf("second mild shortfall: got %d changed=%v, want 31 true", r, changed)
	}
}

func TestRateGovernorClimbsOnlyAfterTwoSaturatingSamples(t *testing.T) {
	g := NewRateGovernor(30, 3, 600)

	if r, changed, _ := g.Observe(31); changed {
		t.Fatalf("one saturating sample moved rate to %d, want no change", r)
	}
	r, changed, _ := g.Observe(30)
	if !changed || r != 34 {
		t.Fatalf("second saturating sample: got %d changed=%v, want 34 true", r, changed)
	}
}

func TestRateGovernorHoldsInsideTheBand(t *testing.T) {
	g := NewRateGovernor(30, 3, 600)
	for _, sample := range []float64{27, 28.5, 27.2} {
		if r, changed, _ := g.Observe(sample); changed {
			t.Fatalf("sample %.1f inside the hold band moved rate to %d", sample, r)
		}
	}
	// Alternating samples must not accumulate a streak in either direction.
	g.Observe(30) // saturating, hits=1
	if r, changed, _ := g.Observe(27); changed {
		t.Fatalf("alternating sample moved rate to %d, want no change", r)
	}
}

func TestRateGovernorRespectsBounds(t *testing.T) {
	g := NewRateGovernor(39, 3, 40)
	g.Observe(40)
	if r, _, _ := g.Observe(40); r != 40 {
		t.Fatalf("climb past ceiling: got %d, want 40", r)
	}

	g = NewRateGovernor(3, 3, 600)
	if r, changed, _ := g.Observe(0.2); changed {
		t.Fatalf("drop below floor moved rate to %d, want no change", r)
	}
}

func TestRateGovernorIgnoresFailedProbes(t *testing.T) {
	g := NewRateGovernor(35, 3, 600)
	if r, changed, why := g.Observe(0); changed || why != "" || r != 35 {
		t.Fatalf("zero measurement: got %d changed=%v why=%q, want 35 false \"\"", r, changed, why)
	}
}

func TestRateGovernorStaysBoundedUnderJitter(t *testing.T) {
	// The samples observed on a real 2.4GHz link at a 35 Mbps declaration.
	g := NewRateGovernor(35, 3, 600)
	for _, sample := range []float64{26.5, 37.0, 35.8, 24.7, 36.6, 26.5, 37.0} {
		before := g.Rate()
		after, _, _ := g.Observe(sample)
		if after < 3 || after > 600 {
			t.Fatalf("rate escaped bounds: %d", after)
		}
		if before != after && (after > before*2 || after*2 < before) {
			t.Fatalf("jitter caused a jump from %d to %d", before, after)
		}
	}
}
