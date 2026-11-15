package core

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

const (
	allocationBytesPerStream = 8 << 20
	allocationWarmup         = 350 * time.Millisecond
	allocationMinWindow      = 500 * time.Millisecond
	allocationIdleWindow     = 2 * time.Second
)

type allocationRunner struct {
	traffic func(context.Context) (allocationTraffic, error)
	gateway func(context.Context) (time.Duration, error)
	wait    func(context.Context, time.Duration) error
	stable  func() error
	measure func(context.Context, int, time.Duration) (allocationObservation, error)
}

// run never writes selectors. Adjacent observations have the same number of
// streams and bytes; the alternating bracket controls for some time drift.
func (r allocationRunner) run(ctx context.Context, current, candidate int, edges bool) (allocationTrial, error) {
	t := allocationTrial{Current: current, Candidate: candidate, CandidateOnEdges: edges}
	if current < 0 || current > BalanceLanes || candidate < 0 || candidate > BalanceLanes || current == candidate {
		return t, fmt.Errorf("invalid candidate")
	}
	if err := r.stable(); err != nil {
		return t, err
	}
	a, err := r.traffic(ctx)
	if err != nil {
		return t, err
	}
	if err = r.wait(ctx, allocationIdleWindow); err != nil {
		return t, err
	}
	b, err := r.traffic(ctx)
	if err != nil {
		return t, err
	}
	if e := allocationTrafficError(a, b, 0, allocationIdleWindow); e != nil {
		return t, e
	}
	if !allocationFlowRepresentative(b.Flows, current, candidate) {
		return t, errAllocationFlowSkew
	}
	t.FlowCounts = b.Flows
	idle, err := r.gateway(ctx)
	if err != nil || !allocationLatencyClean(idle, idle) {
		return t, fmt.Errorf("gateway baseline unavailable or slow")
	}
	order := [3]int{current, candidate, current}
	if edges {
		order = [3]int{candidate, current, candidate}
	}
	for i, share := range order {
		if err = r.stable(); err != nil {
			return t, err
		}
		before, e := r.traffic(ctx)
		if e != nil {
			return t, e
		}
		if !allocationFlowRepresentative(before.Flows, current, candidate) {
			return t, errAllocationFlowSkew
		}
		obs, e := r.measure(ctx, share, idle)
		if e != nil {
			return t, e
		}
		after, e := r.traffic(ctx)
		if e != nil {
			return t, e
		}
		if !allocationFlowRepresentative(after.Flows, current, candidate) {
			return t, errAllocationFlowSkew
		}
		if obs.TotalBytes != int64(BalanceLanes*allocationBytesPerStream) {
			return t, fmt.Errorf("incomplete probe payload")
		}
		if e := allocationTrafficError(before, after, obs.TotalBytes, obs.Duration); e != nil {
			return t, e
		}
		if err = r.stable(); err != nil {
			return t, err
		}
		t.Samples[i] = obs
	}
	return t, nil
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
