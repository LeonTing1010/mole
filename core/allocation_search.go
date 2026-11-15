package core

import (
	"math"
	"slices"
)

// Endpoints and mixed routes share one search and one confirmation history.
type allocationSearch struct {
	preferred, candidate int
	wins                 int
	gains                [3]float64
}

func (p *allocationSearch) advanceCandidate(last int) {
	p.candidate = (last + 1) % (BalanceLanes + 1)
	if p.candidate == p.preferred {
		p.candidate = (p.candidate + 1) % (BalanceLanes + 1)
	}
	p.wins = 0
}

// Require three same-candidate gains >15%, with a median gain of at least 25%.
// These are conservative heuristics, not a statistical confidence interval.
func (p *allocationSearch) recordTrial(candidate int, gain float64) {
	if candidate < 0 || candidate > BalanceLanes || candidate == p.preferred || gain <= 1.15 || math.IsNaN(gain) || math.IsInf(gain, 0) {
		p.wins = 0
		return
	}
	if p.candidate != candidate {
		p.candidate = candidate
		p.wins = 0
	}
	p.gains[p.wins] = gain
	p.wins++
	if p.wins == len(p.gains) {
		gains := p.gains
		slices.Sort(gains[:])
		if gains[1] >= 1.25 {
			p.preferred = candidate
		}
		p.wins = 0
	}
}
