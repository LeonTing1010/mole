package core

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

const allocationSampleEvery = 10 * time.Minute

type allocationEnvironment struct {
	down, rate int
	rung       string
	overflow   uint64
}

// Each sweep downloads nine equal 8 MiB budgets, at most once per ten minutes.
// Fixed forwarders test the allocations without moving any live flow selectors.
func (s *Supervisor) sampleAllocation(p *balancePolicy, now time.Time) {
	if s.allocationSampleFn == nil || utils.ReadLeg() != utils.LegBoth || s.linkBad() ||
		!p.reality.healthy || !p.hy2.healthy || p.reality.slow > 0 || p.hy2.slow > 0 || p.pressure.cooling(now) {
		p.search.wins = 0
		return
	}
	if !p.sampleAttempt.IsZero() && now.Sub(p.sampleAttempt) < allocationSampleEvery {
		return
	}
	p.sampleAttempt = now
	p.reverseScan = !p.reverseScan
	s.stateMu.Lock()
	down, rate := s.state.BandwidthDownMbps, s.state.RealityRateMbps
	s.stateMu.Unlock()
	snapshot := func() floodSnapshot {
		if s.pressureSnapshotFn != nil {
			return s.pressureSnapshotFn()
		}
		return singboxFlood.snapshot()
	}
	event := snapshot()
	rung, err := s.clash.Selection(Hy2ProbeSelectorTag)
	environment := allocationEnvironment{down, rate, rung, event.sequence}
	if environment != p.sampleEnvironment {
		p.search.wins = 0
	}
	stable := func() error {
		if utils.ReadLeg() != utils.LegBoth || s.linkBad() {
			return fmt.Errorf("mode or local link changed during allocation scan")
		}
		s.stateMu.Lock()
		same := down == s.state.BandwidthDownMbps && rate == s.state.RealityRateMbps
		s.stateMu.Unlock()
		live, e := s.clash.Selection(Hy2ProbeSelectorTag)
		if !same || e != nil || live != rung || snapshot().sequence != event.sequence {
			return fmt.Errorf("rate, rung or UDP pressure changed during allocation scan")
		}
		return nil
	}
	var rates [BalanceLanes + 1]float64
	for i := 0; err == nil && i <= BalanceLanes; i++ {
		if err = stable(); err != nil {
			break
		}
		share := i
		if p.reverseScan {
			share = BalanceLanes - i
		}
		rates[share], err = s.allocationSampleFn(share)
	}
	if err == nil {
		err = stable()
	}
	if err != nil {
		p.search.wins = 0
		p.sampleError = err.Error()
		return
	}
	// Search also validates finite, positive rates; never retain incomplete evidence.
	for _, rate := range rates {
		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			p.search.wins = 0
			p.sampleError = "invalid allocation throughput"
			return
		}
	}
	p.search.observe(rates)
	p.sampleRates = rates
	p.sampleEnvironment = environment
	p.sampledAt = time.Now()
	p.sampleError = ""
}

func (s *Supervisor) measureAllocation(share int) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Shutdown cancels in-flight streams instead of waiting for a full scan.
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: allocationTransport{ctx: ctx, base: transport}}
	return allocationBulk(client, share, 1<<20)
}

type allocationTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t allocationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.WithContext(t.ctx))
}
