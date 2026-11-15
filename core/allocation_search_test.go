package core

import (
	"math"
	"testing"
)

// Quality/what: one allocation domain contains pure TCP, mixed, and pure UDP.
func TestAllocationSearchRequiresRepeatedMaterialGainForEveryShare(t *testing.T) {
	// Why: a healthy secondary is not evidence that mixing improves throughput.
	for winner := 0; winner <= BalanceLanes; winner++ {
		initial := (winner + 1) % (BalanceLanes + 1)
		p := allocationSearch{preferred: initial}
		var rates [BalanceLanes + 1]float64
		for i := range rates {
			rates[i] = 100
		}
		rates[winner] = 130
		p.observe(rates)
		if p.preferred != initial {
			t.Fatalf("one sample moved to %d", p.preferred)
		}
		p.observe(rates)
		if p.preferred != winner {
			t.Fatalf("winner %d selected %d", winner, p.preferred)
		}
	}
}

// Quality/what: noise and incomplete comparisons must not become decisions.
func TestAllocationSearchRejectsNoiseInvalidAndInterruptedEvidence(t *testing.T) {
	// Why: partial downloads and short-lived spikes must not steer live traffic.
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		var p allocationSearch
		rates := [BalanceLanes + 1]float64{100, 100, 140, 100, 100, 100, 100, 100, 100}
		p.observe(rates)
		invalid := rates
		invalid[8] = bad
		p.observe(invalid)
		p.observe(rates)
		if p.preferred != 0 {
			t.Fatal("invalid scan failed to break winning streak")
		}
	}
	var p allocationSearch
	rates := [BalanceLanes + 1]float64{100, 110, 100, 100, 100, 100, 100, 100, 100}
	for i := 0; i < 5; i++ {
		p.observe(rates)
	}
	if p.preferred != 0 {
		t.Fatal("noise changed allocation")
	}
}

// Quality/what-x-what: health always constrains the measured preference.
func TestBalanceHealthRestrictsAndInvalidatesPerformancePreference(t *testing.T) {
	// Why: a past fast sample cannot allocate traffic to a failed leg or survive recovery.
	p := balancePolicy{search: allocationSearch{preferred: 5}}
	for i := 0; i < balanceRecoverSamples; i++ {
		p.observe(cleanLeg, cleanLeg)
	}
	// Qualification resets stale preference; a new complete comparison may choose mixed.
	p.search.preferred = 5
	p.observe(cleanLeg, cleanLeg)
	if p.hy2Lanes != 5 {
		t.Fatalf("healthy measured share lost: %d", p.hy2Lanes)
	}
	for i := 0; i < legErrorThreshold; i++ {
		p.observe(cleanLeg, brokenLeg)
	}
	if p.hy2Lanes != 0 || p.search.preferred != 0 {
		t.Fatal("failed UDP retained allocation evidence")
	}
	for i := 0; i < balanceRecoverSamples; i++ {
		p.observe(cleanLeg, cleanLeg)
	}
	if p.hy2Lanes != 0 {
		t.Fatal("recovery resurrected unmeasured dual preference")
	}
}
