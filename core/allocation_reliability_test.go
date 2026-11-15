package core

import (
	"context"
	"math"
	"testing"
	"time"
)

// Quality/what: a trial compares just two allocations with a bracketing control.
func TestAllocationPairedComparisonRejectsDriftAndShortNoise(t *testing.T) {
	// Why: a single lucky maximum among nine short probes cannot justify a switch.
	for _, tc := range []struct {
		name    string
		a, b, c float64
		want    bool
	}{
		{"stable improvement", 100, 130, 105, true},
		{"high baseline drift", 100, 180, 150, false},
		{"small difference", 100, 110, 100, false},
		{"invalid sample", 100, math.NaN(), 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := assessAllocationTrial(allocationTrial{Candidate: 1, Samples: [3]allocationObservation{
				{Mbps: tc.a, Duration: 3 * time.Second}, {Mbps: tc.b, Duration: 3 * time.Second}, {Mbps: tc.c, Duration: 3 * time.Second},
			}})
			if ok != tc.want {
				t.Fatalf("valid improvement=%t, gain=%f", ok, got)
			}
		})
	}
}

// Quality/what: one candidate needs repeated wins before changing any lanes.
func TestAllocationCandidateNeedsThreeCleanPairedWins(t *testing.T) {
	// Why: repeated bracketed improvements reduce dependence on a fortunate sample.
	for candidate := 0; candidate <= BalanceLanes; candidate++ {
		initial := (candidate + 1) % (BalanceLanes + 1)
		p := allocationSearch{preferred: initial}
		for i := 0; i < 2; i++ {
			p.recordTrial(candidate, 1.3)
			if p.preferred != initial {
				t.Fatal("switched without three wins")
			}
		}
		p.recordTrial(candidate, 1.3)
		if p.preferred != candidate {
			t.Fatalf("repeated evidence did not select candidate %d: %d", candidate, p.preferred)
		}
	}
}

// Quality/what: confirmation belongs to one candidate and its median gain.
func TestAllocationConfirmationDoesNotCombineCandidatesOrIsolatedSpikes(t *testing.T) {
	// Why: consolidating exploration and confirmation must preserve evidence boundaries.
	var p allocationSearch
	p.recordTrial(1, 1.4)
	p.recordTrial(1, 1.4)
	p.recordTrial(2, 1.4)
	if p.preferred != 0 || p.wins != 1 {
		t.Fatal("combined different candidates")
	}
	p.recordTrial(2, 1.16)
	p.recordTrial(2, 1.16)
	if p.preferred != 0 {
		t.Fatal("one spike replaced the median gain threshold")
	}
	for i := 0; i < 3; i++ {
		p.recordTrial(2, 1.3)
	}
	if p.preferred != 2 {
		t.Fatal("rejected series prevented later clean confirmations")
	}
}

// Safety/what: a rejected comparison breaks the confirmation streak.
func TestAllocationCandidateRejectsInterveningBadMeasurement(t *testing.T) {
	var p allocationSearch
	p.recordTrial(1, 1.3)
	p.recordTrial(1, 1.3)
	p.recordTrial(1, 0)
	p.recordTrial(1, 1.3)
	if p.preferred != 0 {
		t.Fatal("incomplete trial bridged a confirmation streak")
	}
}

// Safety/what-x-what: health reconciliation must not wait for a slow benchmark.
func TestAllocationCancellationDoesNotBlockOnSlowProbe(t *testing.T) {
	// Why: health failures must override a synthetic optimization measurement.
	var p balancePolicy
	ctx, cancel := context.WithCancel(context.Background())
	p.sampleCancel = cancel
	done := make(chan struct{})
	go func() { <-ctx.Done(); close(done) }()
	start := time.Now()
	p.cancelAllocation()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("health control blocked on sample")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sample was not canceled")
	}
}

// Quality/what: unequal probe durations require time-weighted bracketing.
func TestAllocationPairedComparisonInterpolatesAtCandidateTime(t *testing.T) {
	base := time.Unix(1000, 0)
	trial := allocationTrial{Candidate: 1, Samples: [3]allocationObservation{
		{Mbps: 100, Duration: 3 * time.Second, Midpoint: base},
		{Mbps: 120, Duration: 3 * time.Second, Midpoint: base.Add(time.Second)},
		{Mbps: 120, Duration: 3 * time.Second, Midpoint: base.Add(10 * time.Second)},
	}}
	gain, ok := assessAllocationTrial(trial)
	if !ok || gain < 1.17 || gain > 1.19 {
		t.Fatalf("unweighted or invalid comparison: gain=%v ok=%t", gain, ok)
	}
}

// Quality/audit: measured 2026-11-15 short-probe noise must not force a switch.
func TestAllocationDoesNotPromoteObservedNoisyMeasurements(t *testing.T) {
	// Why: the same 7:1 candidate measured 47.3–144.6 Mbps in the audit.
	var p allocationSearch
	trial := func(a, b, c float64) {
		t1 := allocationTrial{Candidate: 1, Samples: [3]allocationObservation{
			{Mbps: a, Duration: 3 * time.Second}, {Mbps: b, Duration: 3 * time.Second}, {Mbps: c, Duration: 3 * time.Second},
		}}
		gain, _ := assessAllocationTrial(t1)
		p.recordTrial(1, gain)
	}
	trial(113.8, 144.6, 125.3)
	trial(125.3, 47.3, 114.1)
	trial(114.1, 144.6, 125.3)
	if p.preferred != 0 {
		t.Fatal("observed noise forced a route change")
	}
}
