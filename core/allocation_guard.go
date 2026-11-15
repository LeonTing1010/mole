package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errAllocationFlowSkew = errors.New("active connection buckets are insufficient or too skewed")

type allocationTraffic struct {
	Down, Up uint64
	Flows    [BalanceLanes]int
}

// allocationTotals checks actual sing-box counters; missing or reset counters
// must never be silently treated as an idle user.
func (c *ClashClient) allocationTotals(ctx context.Context) (allocationTraffic, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/connections", nil)
	if err != nil {
		return allocationTraffic{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return allocationTraffic{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return allocationTraffic{}, fmt.Errorf("traffic counters HTTP %d", resp.StatusCode)
	}
	var d struct {
		Down        *uint64 `json:"downloadTotal"`
		Up          *uint64 `json:"uploadTotal"`
		Connections []struct {
			Chains []string `json:"chains"`
		} `json:"connections"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return allocationTraffic{}, err
	}
	if d.Down == nil || d.Up == nil {
		return allocationTraffic{}, fmt.Errorf("traffic counters unavailable")
	}
	totals := allocationTraffic{Down: *d.Down, Up: *d.Up}
	for _, c := range d.Connections {
		for _, tag := range c.Chains {
			if !strings.HasPrefix(tag, "flow-") {
				continue
			}
			lane, e := strconv.Atoi(strings.TrimPrefix(tag, "flow-"))
			if e == nil && lane >= 0 && lane < BalanceLanes {
				totals.Flows[lane]++
				break
			}
		}
	}
	return totals, nil
}

// Observe only totals, never log or persist the user's connection metadata.
// The payload is subtracted only if the counters accounted for it completely.
func allocationTrafficError(before, after allocationTraffic, payload int64, d time.Duration) error {
	if d <= 0 || payload < 0 {
		return fmt.Errorf("invalid traffic accounting interval")
	}
	if after.Down < before.Down || after.Up < before.Up {
		return fmt.Errorf("traffic counters reset")
	}
	down, up := after.Down-before.Down, after.Up-before.Up
	if down < uint64(payload) {
		return fmt.Errorf("probe payload not accounted: received %d bytes, counter advanced %d", payload, down)
	}
	extra := down - uint64(payload)
	downLimit := uint64(float64(1<<20) + d.Seconds()*250000)
	upLimit := uint64(float64(1<<19) + d.Seconds()*125000)
	if extra > downLimit {
		return fmt.Errorf("other counted download %d bytes exceeds %d-byte guard", extra, downLimit)
	}
	if up > upLimit {
		return fmt.Errorf("other counted upload %d bytes exceeds %d-byte guard", up, upLimit)
	}
	return nil
}

func allocationLatencyClean(idle, loaded time.Duration) bool {
	if idle <= 0 || idle > 40*time.Millisecond || loaded <= 0 {
		return false
	}
	return loaded <= allocationLatencyLimit(idle)
}

func allocationLatencyLimit(idle time.Duration) time.Duration {
	limit := idle * 4
	if limit < 75*time.Millisecond {
		limit = 75 * time.Millisecond
	}
	return limit
}

// The download runs separately so gateway pressure, mode changes and shutdown
// can cancel it while the supervisor continues to reconcile health.
func guardedAllocationMeasure(ctx context.Context, idle, interval time.Duration, gateway func(context.Context) (time.Duration, error), stable func() error, measure func(context.Context) (allocationObservation, error)) (allocationObservation, error) {
	var empty allocationObservation
	if interval <= 0 {
		return empty, fmt.Errorf("invalid gateway cadence")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		o   allocationObservation
		err error
	}
	done := make(chan result, 1)
	go func() { o, e := measure(ctx); done <- result{o, e} }()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	count := 0
	var worst time.Duration
	finish := func(r result) (allocationObservation, error) {
		if r.err != nil {
			return empty, r.err
		}
		if count == 0 {
			return empty, fmt.Errorf("no gateway observation during download")
		}
		if err := stable(); err != nil {
			return empty, err
		}
		r.o.GatewayWorst = worst
		return r.o, nil
	}
	for {
		select {
		case <-ctx.Done():
			return empty, ctx.Err()
		case r := <-done:
			return finish(r)
		case <-tick.C:
			if err := stable(); err != nil {
				return empty, err
			}
			// A slow control check may have outlived the download. Never label a
			// subsequent idle echo as evidence collected under load.
			select {
			case r := <-done:
				return finish(r)
			default:
			}
			loaded, err := gateway(ctx)
			if err != nil {
				return empty, fmt.Errorf("gateway probe under load unavailable: %w", err)
			}
			if !allocationLatencyClean(idle, loaded) {
				return empty, fmt.Errorf("gateway response under load %s exceeds %s (baseline %s)", loaded, allocationLatencyLimit(idle), idle)
			}
			if loaded > worst {
				worst = loaded
			}
			count++
		}
	}
}

// This is a conservative eligibility check, not proof about future flows or
// byte shares. A snapshot too sparse or skewed cannot validate an equal-stream
// synthetic mixture. Pure endpoints do not depend on bucket proportions.
func allocationFlowRepresentative(counts [BalanceLanes]int, current, candidate int) bool {
	if current < 0 || current > BalanceLanes || candidate < 0 || candidate > BalanceLanes {
		return false
	}
	if (current == 0 || current == BalanceLanes) && (candidate == 0 || candidate == BalanceLanes) {
		return true
	}
	total := 0
	for _, n := range counts {
		if n < 0 {
			return false
		}
		total += n
	}
	if total < 16 {
		return false
	}
	for _, share := range []int{current, candidate} {
		if share == 0 || share == BalanceLanes {
			continue
		}
		udp := 0
		for lane, n := range counts {
			if balanceLaneTarget(lane, share) == Hy2ProbeSelectorTag {
				udp += n
			}
		}
		want := float64(share) / BalanceLanes
		observed := float64(udp) / float64(total)
		tolerance := math.Min(0.1, 0.5*math.Min(want, 1-want))
		if math.Abs(observed-want) > tolerance {
			return false
		}
	}
	return true
}
