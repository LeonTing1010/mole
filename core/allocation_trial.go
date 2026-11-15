package core

import (
	"math"
	"time"
)

// An observation is a complete fixed-budget measurement; short, partial or
// interrupted results must never become evidence.
type allocationObservation struct {
	Mbps                    float64
	TotalMbps               float64
	Duration                time.Duration
	TotalBytes, SteadyBytes int64
	HeaderMedian, HeaderMax time.Duration
	Midpoint                time.Time
	GatewayWorst            time.Duration
}

type allocationTrial struct {
	Current, Candidate int
	// CandidateOnEdges alternates ABA and BAB to avoid a fixed order bias.
	CandidateOnEdges bool
	FlowCounts       [BalanceLanes]int
	Samples          [3]allocationObservation
}

// assessAllocationTrial returns the candidate/current ratio. The outer two
// measurements bracket the middle one; excessive drift invalidates the pair.
func assessAllocationTrial(t allocationTrial) (float64, bool) {
	for _, s := range t.Samples {
		if s.Duration < time.Second || s.Mbps <= 0 || math.IsNaN(s.Mbps) || math.IsInf(s.Mbps, 0) {
			return 0, false
		}
	}
	a, b, c := t.Samples[0].Mbps, t.Samples[1].Mbps, t.Samples[2].Mbps
	if math.Max(a, c) > 1.2*math.Min(a, c) {
		return 0, false
	}
	weight := 0.5
	t0, t1, t2 := t.Samples[0].Midpoint, t.Samples[1].Midpoint, t.Samples[2].Midpoint
	if !(t0.IsZero() && t1.IsZero() && t2.IsZero()) {
		if t0.IsZero() || t1.IsZero() || t2.IsZero() || !t1.After(t0) || !t2.After(t1) {
			return 0, false
		}
		weight = float64(t1.Sub(t0)) / float64(t2.Sub(t0))
	}
	anchor := math.Exp(math.Log(a)*(1-weight) + math.Log(c)*weight)
	gain := b / anchor
	if t.CandidateOnEdges {
		gain = anchor / b
	}
	return gain, gain > 1.15
}

// recordTrial requires three same-candidate, individually material wins and a
// median gain of at least 25%. These are conservative heuristics, not a formal
// statistical confidence interval or proof of an application-level benefit.
func (p *allocationSearch) recordTrial(candidate int, gain float64, valid bool) {
	if candidate < 0 || candidate > BalanceLanes || candidate == p.preferred || !valid || gain <= 1.15 || math.IsNaN(gain) || math.IsInf(gain, 0) {
		p.wins = 0
		p.gains = [3]float64{}
		return
	}
	if p.pending != candidate {
		p.pending = candidate
		p.wins = 0
		p.gains = [3]float64{}
	}
	p.gains[p.wins] = gain
	p.wins++
	if p.wins == 3 {
		gains := p.gains
		if gains[0] > gains[1] {
			gains[0], gains[1] = gains[1], gains[0]
		}
		if gains[1] > gains[2] {
			gains[1], gains[2] = gains[2], gains[1]
		}
		if gains[0] > gains[1] {
			gains[0], gains[1] = gains[1], gains[0]
		}
		if gains[1] >= 1.25 {
			p.preferred = candidate
		}
		p.wins = 0
		p.gains = [3]float64{}
	}
}
