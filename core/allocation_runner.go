package core

import (
	"context"
	"fmt"
	"time"
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
