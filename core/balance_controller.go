package core

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func initialBalanceStatus(supported bool) *utils.BalanceStatus {
	if !supported {
		return nil
	}
	return &utils.BalanceStatus{Supported: true}
}

func (s *Supervisor) runBalance(ctx context.Context) {
	if err := s.waitClashReady(ctx, 10*time.Second); err != nil {
		log.Printf("dual routing: %v", err)
	}
	var policy balancePolicy
	t := time.NewTicker(s.keepaliveEvery)
	defer t.Stop()
	for {
		s.balanceOnce(&policy)
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
	}
}

func (s *Supervisor) balanceOnce(p *balancePolicy) {
	if utils.ReadLeg() != utils.LegBoth {
		s.stateMu.Lock()
		enabled := s.state.Balance != nil && s.state.Balance.Enabled
		s.stateMu.Unlock()
		if !enabled {
			return
		}
		if _, err := ApplyBalancedLanes(s.clash, -1); err != nil {
			s.balanceError(err)
			return
		}
		*p = balancePolicy{}
		s.publishBalance(&utils.BalanceStatus{Supported: true})
		s.resetKeepaliveCount()
		return
	}
	if s.linkBad() {
		p.pressure.cleanSince = time.Time{}
		return
	}
	target := s.ladderTarget()
	s.stateMu.Lock()
	down := s.state.BandwidthDownMbps
	s.stateMu.Unlock()
	if s.schedule.HasRung(down) {
		target = BandwidthRungTag(down)
	}
	live, err := s.clash.Selection(Hy2ProbeSelectorTag)
	if err != nil {
		s.balanceError(err)
		return
	}
	now := time.Now()
	event := singboxFlood.snapshot()
	if s.pressureSnapshotFn != nil {
		event = s.pressureSnapshotFn()
	}
	oldCap := p.pressure.capDown
	guarded := p.pressure.target(event, now, BandwidthRungMbps(live), BandwidthRungMbps(target), s.schedule,
		p.hy2.healthy && p.hy2.slow == 0)
	if guarded > 0 {
		target = BandwidthRungTag(guarded)
	}
	if oldCap != p.pressure.capDown {
		log.Printf("dual routing: local UDP pressure ceiling %d -> %d Mbps down (paired up %d Mbps)", oldCap, p.pressure.capDown, s.schedule.RungUp(guarded))
	}
	if live != target {
		if err := s.clash.SelectProxy(Hy2ProbeSelectorTag, target); err != nil {
			s.balanceError(err)
			return
		}
	}
	// Pressure cooling: when UDP writes are being refused by the kernel, we
	// temporarily drain new UDP flows before running probes so the probe
	// load does not push an already-overloaded socket further. This is a
	// short-lived pre-probe precaution only — observe() below will compute
	// the correct lane allocation from fresh probe data, and the subsequent
	// ApplyBalancedLanes write will restore them.
	if p.pressure.cooling(now) && p.reality.healthy {
		if _, err := ApplyBalancedLanes(s.clash, 0); err != nil {
			s.balanceError(err)
			return
		}
	}
	var reality, hy2 legMeasurement
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reality.delay, reality.err = s.probeLeg(BrutalKeeperPort) }()
	go func() { defer wg.Done(); hy2.delay, hy2.err = s.probeLeg(LegGatePort) }()
	wg.Wait()
	p.observe(reality, hy2)
	if utils.ReadLeg() != utils.LegBoth || s.linkBad() {
		p.pressure.cleanSince = time.Time{}
		return
	}
	changed, err := ApplyBalancedLanes(s.clash, p.hy2Lanes)
	if err != nil {
		s.balanceError(err)
		return
	}
	main := RealityLegTag
	if p.hy2Lanes >= BalanceLanes/2 {
		main = target
	}
	var auxiliaryError error
	if current, err := s.clash.Selection(ProxySelectorTag); err != nil {
		auxiliaryError = err
	} else if current != main {
		auxiliaryError = s.clash.SelectProxy(ProxySelectorTag, main)
	}
	status := &utils.BalanceStatus{
		Supported: true, Enabled: true, RealityLanes: BalanceLanes - p.hy2Lanes, Hy2Lanes: p.hy2Lanes,
		RealityHealthy: p.reality.healthy, Hy2Healthy: p.hy2.healthy,
		LastCheckedAt:   time.Now(),
		RealityDegraded: p.reality.healthy && p.reality.slow > 0, Hy2Degraded: p.hy2.healthy && p.hy2.slow > 0,
		RealityLatencyMs: reality.delay.Milliseconds(), Hy2LatencyMs: hy2.delay.Milliseconds(),
		Hy2DownMbps: BandwidthRungMbps(target), Hy2UpMbps: s.schedule.RungUp(BandwidthRungMbps(target)),
		UDPPressureCapMbps: p.pressure.capDown, UDPCooldownUntil: p.pressure.coolUntil,
	}
	if auxiliaryError != nil {
		status.Error = "auxiliary route reconciliation pending: " + auxiliaryError.Error()
	}
	if reality.err != nil {
		status.RealityError = reality.err.Error()
	}
	if hy2.err != nil {
		status.Hy2Error = hy2.err.Error()
	}
	s.publishBalance(status)
	if changed {
		log.Printf("dual routing: new flows TCP %d/%d, UDP %d/%d", status.RealityLanes, BalanceLanes, status.Hy2Lanes, BalanceLanes)
		s.resetKeepaliveCount()
	}
}

func (s *Supervisor) publishBalance(status *utils.BalanceStatus) {
	s.stateMu.Lock()
	s.state.Balance = status // Immutable after publication; snapshots can share it.
	s.stateMu.Unlock()
	_ = utils.WriteState(s.snapshot())
}

func (s *Supervisor) balanceError(err error) {
	s.stateMu.Lock()
	status := utils.BalanceStatus{Supported: true}
	if s.state.Balance != nil {
		status = *s.state.Balance
	}
	s.stateMu.Unlock()
	status.Error = err.Error()
	s.publishBalance(&status)
	log.Printf("dual routing: reconciliation pending: %v", err)
}
