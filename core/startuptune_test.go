package core

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestStartupTuneRequiresRepeatableDownloadImprovement(t *testing.T) {
	// Why: one fast burst or an incomplete transfer cannot justify moving real traffic.
	for _, test := range []struct {
		name  string
		rates []float64
		want  int
	}{
		{"repeatable gain", []float64{100, 130, 108, 109, 132, 101}, 300},
		{"one fast burst", []float64{100, 180, 108, 109, 104, 101}, 600},
		{"failed candidate", []float64{100, 180, 108, 109, 0, 101}, 600},
		{"failed baseline", []float64{0, 180, 108, 109, 160, 101}, 600},
	} {
		t.Run(test.name, func(t *testing.T) {
			var order []int
			got := measureStartupCeiling(context.Background(), []int{600, 300, 200}, func(_ context.Context, rung int) (float64, error) {
				rate := test.rates[len(order)]
				order = append(order, rung)
				if rate == 0 {
					return 0, errors.New("incomplete transfer")
				}
				return rate, nil
			})
			if got != test.want || !slices.Equal(order, []int{600, 300, 200, 200, 300, 600}) {
				t.Fatalf("selected %d with order %v, want %d", got, order, test.want)
			}
		})
	}
}

func TestStartupTuneCancellationKeepsSavedRung(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := measureStartupCeiling(ctx, []int{600, 300}, func(_ context.Context, rung int) (float64, error) {
		if rung != 600 {
			t.Fatal("probed a candidate after cancellation")
		}
		cancel()
		return 100, nil
	})
	if got != 600 {
		t.Fatalf("selected %d after cancellation", got)
	}
}

func TestStartupTuneRestoresOnlyAvailableRungs(t *testing.T) {
	schedule := BandwidthSchedule{OffDown: 150, PeakDown: 600}
	now := time.Now()
	_, _, clock := schedule.Profile(now)
	for _, test := range []struct{ seed, cached, want int }{
		{600, 300, 300},
		{600, 999, 600},
		{999, 0, clock},
	} {
		if got := schedule.StartupTuneBaseline(test.seed, test.cached, now); got != test.want {
			t.Fatalf("seed %d, cached %d: got %d, want %d", test.seed, test.cached, got, test.want)
		}
	}
	if got := startupTuneCandidates(schedule.Rungs(), 600); !slices.Equal(got, []int{600, 300, 200}) {
		t.Fatalf("top rung comparison got %v", got)
	}
}
