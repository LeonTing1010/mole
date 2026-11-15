package core

import "math"

// allocationSearch compares complete equal-budget sweeps. Single transports
// are simply the endpoints of the same allocation space as mixed traffic.
type allocationSearch struct {
	preferred int
	pending   int
	wins      int
}

func (p *allocationSearch) observe(rates [BalanceLanes + 1]float64) {
	for _, rate := range rates {
		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			p.wins = 0
			return
		}
	}
	best := p.preferred
	for share, rate := range rates {
		if rate > rates[best] {
			best = share
		}
	}
	if rates[best] <= rates[p.preferred]*1.15 {
		p.wins = 0
		return
	}
	if p.pending != best {
		p.pending = best
		p.wins = 0
	}
	p.wins++
	if p.wins >= 2 {
		p.preferred = best
		p.wins = 0
	}
}
