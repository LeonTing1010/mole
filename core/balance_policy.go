package core

import (
	"fmt"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

const (
	BalanceLanes          = 8
	balanceRecoverSamples = legGateProbes
	balanceSlowSamples    = 3
	balancePortBlock      = 32

	// DefaultHy2Lanes is the fixed hy2 allocation when both legs are healthy.
	// Reality is consistently 2x faster and more stable than hy2 on this VPS,
	// so we bias heavily toward it. hy2 still keeps 2 lanes (25%) as warm
	// fallback — enough to carry traffic instantly if reality fails, without
	// wasting bandwidth on a slower leg during normal operation.
	DefaultHy2Lanes = 2
)

func BalanceLaneTag(lane int) string { return fmt.Sprintf("flow-%d", lane) }

// Repeated small port bands avoid putting a whole ephemeral-port range onto
// one leg. Source-port matching is fixed for the life of a connection.
func BalancePortRanges(lane int) []string {
	var ranges []string
	if lane < 0 || lane >= BalanceLanes {
		return ranges
	}
	for low := lane * balancePortBlock; low <= 65535; low += BalanceLanes * balancePortBlock {
		high := low + balancePortBlock - 1
		if low == 0 {
			low = 1
			high = balancePortBlock - 1
		}
		if high > 65535 {
			high = 65535
		}
		ranges = append(ranges, fmt.Sprintf("%d:%d", low, high))
		// Restore the block origin after excluding source port zero.
		if low == 1 {
			low = 0
		}
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
			l.good = 0
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
	reality, hy2 balanceLeg
	hy2Lanes     int
	pressure     udpPressure
}

func (p *balancePolicy) observe(reality, hy2 legMeasurement) {
	p.reality.observe(reality)
	p.hy2.observe(hy2)
	switch {
	case !p.hy2.healthy:
		// hy2 down (includes both-down): pin everything to REALITY. TCP
		// retransmits ride out the loss that kills UDP, and keeping the
		// last allocation would pin all new connections to a dead leg.
		p.setLanes(0)
	case !p.reality.healthy:
		p.setLanes(BalanceLanes)
	default:
		// Both healthy — fixed biased split. No bulk sample, no ratio,
		// no EMA. sing-box BBR + HY2 ladder handle congestion control;
		// we just decide how many NEW connections start on each leg.
		p.setLanes(DefaultHy2Lanes)
	}
}

func (p *balancePolicy) setLanes(n int) {
	p.hy2Lanes = n
}
