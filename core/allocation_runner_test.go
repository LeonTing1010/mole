package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Quality/what: each paired trial alternates order without changing selectors.
func TestAllocationTrialUsesCounterbalancedMatchedProbes(t *testing.T) {
	for _, edges := range []bool{false, true} {
		var order []int
		totals := allocationTraffic{Flows: [8]int{3, 3, 3, 3, 3, 3, 3, 3}}
		r := allocationRunner{
			traffic: func(context.Context) (allocationTraffic, error) { totals.Down += 64 << 20; return totals, nil },
			gateway: func(context.Context) (time.Duration, error) { return 4 * time.Millisecond, nil },
			wait:    func(context.Context, time.Duration) error { return nil },
			stable:  func() error { return nil },
			measure: func(_ context.Context, share int, _ time.Duration) (allocationObservation, error) {
				order = append(order, share)
				rate := 100.
				if share == 1 {
					rate = 130
				}
				return allocationObservation{Mbps: rate, Duration: 3 * time.Second, TotalBytes: 64 << 20}, nil
			},
		}
		// The first idle counter pair must show no background traffic.
		calls := 0
		base := r.traffic
		r.traffic = func(c context.Context) (allocationTraffic, error) {
			calls++
			if calls == 2 {
				return totals, nil
			}
			return base(c)
		}
		trial, err := r.run(context.Background(), 0, 1, edges)
		if err != nil {
			t.Fatal(err)
		}
		want := []int{0, 1, 0}
		if edges {
			want = []int{1, 0, 1}
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("order %v want %v", order, want)
			}
		}
		if _, ok := assessAllocationTrial(trial); !ok {
			t.Fatal("valid gain rejected")
		}
	}
}

// Safety/what-x-what: no verdict may use incomplete, busy or changed conditions.
func TestAllocationTrialStopsForBackgroundFailureAndChangedMode(t *testing.T) {
	for _, cause := range []string{"busy", "counter", "gateway", "sample", "mode"} {
		t.Run(cause, func(t *testing.T) {
			calls := 0
			stableCalls := 0
			totals := allocationTraffic{Flows: [8]int{3, 3, 3, 3, 3, 3, 3, 3}}
			r := allocationRunner{
				traffic: func(context.Context) (allocationTraffic, error) {
					calls++
					if cause == "counter" {
						return totals, errors.New("missing counters")
					}
					if calls > 1 {
						totals.Down += 64 << 20
					}
					return totals, nil
				},
				gateway: func(context.Context) (time.Duration, error) {
					if cause == "gateway" {
						return 200 * time.Millisecond, nil
					}
					return 3 * time.Millisecond, nil
				},
				wait: func(context.Context, time.Duration) error { return nil },
				stable: func() error {
					stableCalls++
					if cause == "mode" && stableCalls > 1 {
						return errors.New("mode changed")
					}
					return nil
				},
				measure: func(context.Context, int, time.Duration) (allocationObservation, error) {
					if cause == "sample" {
						return allocationObservation{}, errors.New("partial payload")
					}
					return allocationObservation{Mbps: 100, Duration: 3 * time.Second, TotalBytes: 64 << 20}, nil
				},
			}
			if cause != "busy" {
				prev := r.traffic
				calls2 := 0
				r.traffic = func(c context.Context) (allocationTraffic, error) {
					calls2++
					if calls2 == 2 {
						return totals, nil
					}
					return prev(c)
				}
			}
			if _, err := r.run(context.Background(), 0, 1, false); err == nil {
				t.Fatal("unsafe trial qualified")
			}
		})
	}
}
