package core

import "time"

const (
	udpPressureFresh     = time.Minute
	udpPressureDropEvery = time.Minute
	udpPressureCooldown  = 30 * time.Second
	udpPressureRecover   = 15 * time.Minute
)

// A local UDP write refusal is pressure evidence, not a measurement of WAN
// capacity. Rate-limit descents because one failed session can close many flows.
// Use only existing rungs; the configured/scheduled ceiling remains the maximum.
type udpPressure struct {
	sequence                        uint64
	capDown                         int
	lastDrop, coolUntil, cleanSince time.Time
}

func (p *udpPressure) cooling(now time.Time) bool { return now.Before(p.coolUntil) }

func (p *udpPressure) target(event floodSnapshot, now time.Time, live, desired int, s BandwidthSchedule, healthy bool) int {
	fresh := event.sequence != p.sequence && !event.last.IsZero() &&
		!event.last.After(now) && now.Sub(event.last) <= udpPressureFresh
	p.sequence = event.sequence
	if fresh {
		p.coolUntil = now.Add(udpPressureCooldown)
		p.cleanSince = now
		if p.lastDrop.IsZero() || now.Sub(p.lastDrop) >= udpPressureDropEvery {
			base := desired
			if s.HasRung(live) {
				base = min(base, live)
			}
			if p.capDown > 0 {
				base = min(base, p.capDown)
			}
			p.capDown = s.StepDown(base, 1)
			p.lastDrop = now
		}
	}
	if !healthy {
		p.cleanSince = time.Time{}
	} else if p.cleanSince.IsZero() {
		p.cleanSince = now
	}
	if p.capDown > 0 && healthy && !p.cooling(now) && now.Sub(p.cleanSince) >= udpPressureRecover {
		if p.capDown >= desired {
			p.capDown = 0
		} else {
			for _, rung := range s.Rungs() {
				if rung > p.capDown {
					p.capDown = min(rung, desired)
					break
				}
			}
		}
		p.cleanSince = now
	}
	if p.capDown > 0 {
		return min(desired, p.capDown)
	}
	return desired
}
