package core

import (
	"testing"
)

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
	if p.lanes() != 5 {
		t.Fatalf("healthy measured share lost: %d", p.lanes())
	}
	for i := 0; i < legErrorThreshold; i++ {
		p.observe(cleanLeg, brokenLeg)
	}
	if p.lanes() != 0 || p.search.preferred != 0 {
		t.Fatal("failed UDP retained allocation evidence")
	}
	for i := 0; i < balanceRecoverSamples; i++ {
		p.observe(cleanLeg, cleanLeg)
	}
	if p.lanes() != 0 {
		t.Fatal("recovery resurrected unmeasured dual preference")
	}
}
