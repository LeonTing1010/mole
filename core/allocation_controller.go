package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Three 64 MiB phases every 30 minutes: at most 384 MiB/hour, and only in
// explicitly selected both mode, with idle and gateway safeguards.
const allocationSampleEvery = 30 * time.Minute

type allocationEnvironment struct {
	down, rate            int
	rung, gateway, uplink string
	overflow              uint64
}
type allocationScanResult struct {
	trial allocationTrial
	env   allocationEnvironment
	err   error
}

func (s *Supervisor) allocationEnv() (allocationEnvironment, error) {
	s.stateMu.Lock()
	down, rate, gw := s.state.BandwidthDownMbps, s.state.RealityRateMbps, s.state.LinkGateway
	s.stateMu.Unlock()
	rung, err := s.clash.Selection(Hy2ProbeSelectorTag)
	if err != nil {
		return allocationEnvironment{}, err
	}
	uplinkFn := s.allocationUplinkFn
	if uplinkFn == nil {
		uplinkFn = currentUplink
	}
	uplink, err := uplinkFn()
	if err != nil || uplink == "" {
		return allocationEnvironment{}, fmt.Errorf("physical uplink unavailable")
	}
	event := singboxFlood.snapshot()
	if s.pressureSnapshotFn != nil {
		event = s.pressureSnapshotFn()
	}
	return allocationEnvironment{down: down, rate: rate, rung: rung, gateway: gw, uplink: uplink, overflow: event.sequence}, err
}

// Confirm that a trial still describes the active physical path and the exact
// applied allocation, rather than an intended but failed selector write.
func (s *Supervisor) stableAllocation(env allocationEnvironment, current int) error {
	if utils.ReadLeg() != utils.LegBoth || s.linkBad() {
		return fmt.Errorf("mode or local link changed")
	}
	live, err := s.allocationEnv()
	if err != nil || env != live {
		return fmt.Errorf("rate, rung, gateway or UDP pressure changed")
	}
	for lane := 0; lane < BalanceLanes; lane++ {
		want := balanceLaneTarget(lane, current)
		got, e := s.clash.Selection(BalanceLaneTag(lane))
		if e != nil || got != want {
			return fmt.Errorf("allocation selectors changed or are not reconciled")
		}
	}
	return nil
}

// sampleAllocation only starts or reaps background work. No download may block
// the health loop; mode or health changes cancel the outstanding operation.
func (s *Supervisor) sampleAllocation(p *balancePolicy, now time.Time, allowStart bool) {
	eligible := s.allocationRunFn != nil && utils.ReadLeg() == utils.LegBoth && !s.linkBad() &&
		p.reality.healthy && p.hy2.healthy && p.reality.bad == 0 && p.hy2.bad == 0 &&
		p.reality.slow == 0 && p.hy2.slow == 0 && !p.pressure.cooling(now)
	if !eligible {
		p.search.recordTrial(p.candidate, 0, false)
		p.cancelAllocation()
		return
	}
	if p.sampleJob != nil {
		select {
		case res := <-p.sampleJob:
			p.cancelAllocation()
			if res.err == nil {
				res.err = s.stableAllocation(res.env, res.trial.Current)
			}
			if res.err == nil && (res.trial.Current != p.search.preferred || res.trial.Candidate != p.candidate) {
				res.err = fmt.Errorf("allocation changed during trial")
			}
			if res.err == nil {
				checkCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				currentFlows, e := s.clash.allocationTotals(checkCtx)
				cancel()
				if e != nil {
					res.err = e
				} else if !allocationFlowRepresentative(currentFlows.Flows, res.trial.Current, res.trial.Candidate) {
					res.err = errAllocationFlowSkew
				}
			}
			if res.err != nil {
				p.search.recordTrial(p.candidate, 0, false)
				p.sampleError = res.err.Error()
				if errors.Is(res.err, errAllocationFlowSkew) {
					p.candidate = nextAllocationCandidate(p.candidate, p.search.preferred)
				}
				return
			}
			gain, qualified := assessAllocationTrial(res.trial)
			previous := p.search.preferred
			p.search.recordTrial(p.candidate, gain, qualified)
			evidence := &utils.AllocationEvidence{CurrentHy2Lanes: res.trial.Current, CandidateHy2Lanes: res.trial.Candidate, CandidateOnEdges: res.trial.CandidateOnEdges, GainPct: (gain - 1) * 100, Qualified: qualified, ConsecutiveWins: p.search.wins, ProbePayloadBytes: 3 * int64(BalanceLanes*allocationBytesPerStream)}
			for _, x := range res.trial.Samples {
				evidence.RatesMbps = append(evidence.RatesMbps, x.Mbps)
				evidence.HeaderMedianMs = append(evidence.HeaderMedianMs, x.HeaderMedian.Milliseconds())
				evidence.LoadedGatewayWorstMs = append(evidence.LoadedGatewayWorstMs, x.GatewayWorst.Milliseconds())
			}
			switch {
			case p.search.preferred != previous:
				evidence.Decision = "selected"
			case qualified && p.search.wins > 0:
				evidence.Decision = "confirming"
			default:
				evidence.Decision = "hold"
			}
			evidence.FlowConnections = append([]int(nil), res.trial.FlowCounts[:]...)
			if gain <= 0 {
				evidence.GainPct = 0
			}
			p.sampleEvidence = evidence
			p.timeLastSample = now
			p.sampleEnvironment = res.env
			p.sampleError = ""
			// A losing/invalid comparison or a completed three-win series advances to
			// the next candidate; no noisy nine-way winner is chosen.
			if !qualified || p.search.wins == 0 {
				p.candidate = nextAllocationCandidate(res.trial.Candidate, p.search.preferred)
			}
		default:
			return
		}
	}
	if !allowStart {
		return
	}
	if !p.sampleAttempt.IsZero() && now.Sub(p.sampleAttempt) < allocationSampleEvery {
		return
	}
	p.sampleAttempt = now
	env, err := s.allocationEnv()
	if err == nil {
		err = s.stableAllocation(env, p.search.preferred)
	}
	if err != nil {
		p.search.recordTrial(p.candidate, 0, false)
		p.sampleError = err.Error()
		return
	}
	if env != p.sampleEnvironment {
		p.search.recordTrial(p.candidate, 0, false)
	}
	if p.candidate < 0 || p.candidate > BalanceLanes || p.candidate == p.search.preferred {
		p.candidate = nextAllocationCandidate(p.search.preferred, p.search.preferred)
	}
	current, candidate, edges := p.search.preferred, p.candidate, p.reverseScan
	p.reverseScan = !p.reverseScan
	ctx, cancel := context.WithCancel(context.Background())
	p.sampleCancel = cancel
	job := make(chan allocationScanResult, 1)
	p.sampleJob = job
	go func() {
		defer cancel()
		go func() {
			select {
			case <-s.stopCh:
				cancel()
			case <-ctx.Done():
			}
		}()
		stable := func() error {
			if e := ctx.Err(); e != nil {
				return e
			}
			return s.stableAllocation(env, current)
		}
		trial, e := s.allocationRunFn(ctx, current, candidate, edges, stable)
		job <- allocationScanResult{trial: trial, env: env, err: e}
	}()
}

func nextAllocationCandidate(last, current int) int {
	for i := 1; i <= BalanceLanes+1; i++ {
		n := (last + i) % (BalanceLanes + 1)
		if n != current {
			return n
		}
	}
	return current
}

func (s *Supervisor) runAllocationTrial(ctx context.Context, current, candidate int, edges bool, stable func() error) (allocationTrial, error) {
	var gateway, ifname string
	ping := func(ctx context.Context) (time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		g, i, err := utils.DefaultGateway()
		if err != nil || g == "" {
			return 0, fmt.Errorf("gateway unavailable")
		}
		s.stateMu.Lock()
		expectedGateway := s.state.LinkGateway
		s.stateMu.Unlock()
		if expectedGateway == "" || g != expectedGateway {
			return 0, fmt.Errorf("gateway state changed")
		}
		if gateway != "" && (gateway != g || ifname != i) {
			return 0, fmt.Errorf("gateway changed")
		}
		gateway, ifname = g, i
		return echoGateway(gateway, ifname, 750*time.Millisecond)
	}
	wait := func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	runner := allocationRunner{traffic: s.clash.allocationTotals, gateway: ping, wait: wait, stable: stable}
	runner.measure = func(ctx context.Context, share int, idle time.Duration) (allocationObservation, error) {
		return guardedAllocationMeasure(ctx, idle, time.Second, ping, stable, func(ctx context.Context) (allocationObservation, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			transport := &http.Transport{DisableKeepAlives: true, Proxy: nil}
			defer transport.CloseIdleConnections()
			client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
			return measureAllocationURLs(ctx, client, allocationURLs(share), allocationBytesPerStream, allocationWarmup, allocationMinWindow)
		})
	}
	return runner.run(ctx, current, candidate, edges)
}
