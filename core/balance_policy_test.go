package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var cleanLeg = legMeasurement{delay: 200 * time.Millisecond}
var brokenLeg = legMeasurement{err: errors.New("timeout")}

func TestBalanceRecoveryRequiresConsecutiveCleanSamples(t *testing.T) {
	var l balanceLeg
	for i := 0; i < balanceRecoverSamples*2; i++ {
		l.observe(cleanLeg)
		l.observe(brokenLeg)
		if l.healthy {
			t.Fatal("interleaved errors qualified a recovering leg")
		}
	}
	for i := 0; i < balanceRecoverSamples; i++ {
		l.observe(cleanLeg)
	}
	if !l.healthy {
		t.Fatal("clean recovery did not qualify")
	}
	l.observe(brokenLeg)
	if !l.healthy {
		t.Fatal("a single error withdrew an already qualified leg")
	}
}

// Quality/what: three consecutive errors withdraw a leg; recovery comes back to fixed split.
func TestBalanceObserveFixedSplit(t *testing.T) {
	var p balancePolicy

	// Both unqualified → 0 lanes (no healthy yet)
	p.observe(cleanLeg, cleanLeg)
	p.observe(cleanLeg, cleanLeg)
	if p.hy2Lanes != 0 {
		t.Fatal("unqualified legs should not get lanes yet")
	}

	// After balanceRecoverSamples → both healthy → fixed split
	for i := 0; i < balanceRecoverSamples; i++ {
		p.observe(cleanLeg, cleanLeg)
	}
	if p.hy2Lanes != DefaultHy2Lanes {
		t.Fatalf("both healthy should give fixed split %d, got %d", DefaultHy2Lanes, p.hy2Lanes)
	}

	// hy2 unhealthy → 0 lanes (3 consecutive errors)
	p.observe(cleanLeg, brokenLeg)
	p.observe(cleanLeg, brokenLeg)
	p.observe(cleanLeg, brokenLeg)
	if p.hy2Lanes != 0 {
		t.Fatal("broken hy2 should get 0 lanes after 3 errors")
	}

	// reality unhealthy → all lanes to hy2
	var p2 balancePolicy
	for i := 0; i < balanceRecoverSamples; i++ {
		p2.observe(cleanLeg, cleanLeg)
	}
	p2.observe(brokenLeg, cleanLeg)
	p2.observe(brokenLeg, cleanLeg)
	p2.observe(brokenLeg, cleanLeg)
	if p2.hy2Lanes != BalanceLanes {
		t.Fatal("broken reality should give all lanes to hy2")
	}

	// Both broken → all lanes to REALITY (TCP survives loss that kills UDP)
	var p3 balancePolicy
	for i := 0; i < balanceRecoverSamples; i++ {
		p3.observe(cleanLeg, cleanLeg)
	}
	p3.observe(brokenLeg, brokenLeg)
	p3.observe(brokenLeg, brokenLeg)
	p3.observe(brokenLeg, brokenLeg)
	if p3.hy2Lanes != 0 {
		t.Fatal("both broken should pin all lanes to reality")
	}
}

// Quality/what: bucket shares must account for every connection exactly once.
func TestBalancePortBucketsCoverTheEntireSourcePortSpace(t *testing.T) {
	counts := make([]int, 65536)
	for lane := 0; lane < BalanceLanes; lane++ {
		for _, r := range BalancePortRanges(lane) {
			lo, hi := 0, 0
			if _, err := fmt.Sscanf(r, "%d:%d", &lo, &hi); err != nil {
				t.Fatal(err)
			}
			for port := lo; port <= hi; port++ {
				counts[port]++
			}
		}
	}
	for port := 1; port <= 65535; port++ {
		if counts[port] != 1 {
			t.Fatalf("source port %d matches %d lanes", port, counts[port])
		}
	}
}
