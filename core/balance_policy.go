package core

import (
	"context"
	"fmt"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

const (
	BalanceLanes          = 8
	balanceRecoverSamples = legGateProbes
	balanceSlowSamples    = 3
	balancePortBlock      = 32

	// Start with REALITY until complete comparisons demonstrate a benefit.
	DefaultHy2Lanes = 0
)

func BalanceLaneTag(lane int) string { return fmt.Sprintf("flow-%d", lane) }

// balanceLaneTarget is shared by live selectors and synthetic measurements.
func balanceLaneTarget(lane, hy2Lanes int) string {
	if hy2Lanes < 0 {
		return ProxySelectorTag
	}
	if (lane+1)*hy2Lanes/BalanceLanes > lane*hy2Lanes/BalanceLanes {
		return Hy2ProbeSelectorTag
	}
	return RealityLegTag
}

// Repeated small port bands avoid putting a whole ephemeral-port range onto
// one leg. Source-port matching is fixed for the life of a connection.
func BalancePortRanges(lane int) []string {
	var ranges []string
	if lane < 0 || lane >= BalanceLanes {
		return ranges
	}
	for low := lane * balancePortBlock; low <= 65535; low += BalanceLanes * balancePortBlock {
		ranges = append(ranges, fmt.Sprintf("%d:%d", max(1, low), min(65535, low+balancePortBlock-1)))
	}
	return ranges
}

type balanceLeg struct {
	good    int
	bad     int
	slow    int
	healthy bool
}

// legErrorThreshold — consecutive probe errors before marking unhealthy.
// Three is deliberately conservative: each probe fires every ~30s, so 3
// errors = ~90s of actual failure. This prevents transient sing-box
// "dial tcp i/o timeout" glitches (common on this VPS every 10-15 min)
// from triggering a full leg withdrawal. One leg flipping to unhealthy
// forces all traffic onto the other — if that flips back 30s later, the
// lane churn defeats the purpose of dual routing.
const legErrorThreshold = 3

func (l *balanceLeg) observe(m legMeasurement) {
	if m.err != nil {
		l.bad++
		l.good = 0 // Recovery requires consecutive clean probes, even before withdrawal.
		l.slow = 0
		if l.bad >= legErrorThreshold {
			l.healthy = false
		}
		return
	}
	l.bad = 0
	if m.delay > time.Duration(utils.DialSlowMs)*time.Millisecond {
		if l.slow < balanceSlowSamples {
			l.slow++
		}
		l.good = 0
		if l.slow >= balanceSlowSamples {
			l.healthy = false
		}
		return
	}
	l.slow = 0
	if l.good < balanceRecoverSamples {
		l.good++
	}
	l.healthy = l.healthy || l.good >= balanceRecoverSamples
}

type balancePolicy struct {
	reality, hy2             balanceLeg
	pressure                 udpPressure
	search                   allocationSearch
	sampleAttempt, sampledAt time.Time
	sampleError              string
	sampleEnvironment        allocationEnvironment
	candidateOnEdges         bool
	sampleCancel             context.CancelFunc
	sampleJob                chan allocationResult
	sampleEvidence           *utils.AllocationEvidence
}

func (p *balancePolicy) observe(reality, hy2 legMeasurement) {
	p.reality.observe(reality)
	p.hy2.observe(hy2)
	if !p.reality.healthy || !p.hy2.healthy {
		p.search = allocationSearch{}
	}
}

// Health constrains the confirmed preference. Both-down retains REALITY so
// new connections never remain pinned to a failed UDP leg.
func (p *balancePolicy) lanes() int {
	if !p.hy2.healthy {
		return 0
	}
	if !p.reality.healthy {
		return BalanceLanes
	}
	return p.search.preferred
}

func (p *balancePolicy) cancelAllocation() {
	if p.sampleCancel != nil {
		p.sampleCancel()
		p.sampleCancel = nil
	}
	p.sampleJob = nil
}

func (p *balancePolicy) discardAllocation() {
	p.search.wins = 0
	p.cancelAllocation()
}
