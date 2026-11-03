package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Quality/what: one unavailable origin must not disqualify a working tunnel.
func TestPerformanceEgressAcceptsIndependentHealthyOrigin(t *testing.T) {
	// Why: a single Cloudflare failure is not proof that the transport is down.
	targets := []LegEgressTarget{{Host: "origin-a"}, {Host: "origin-b"}}
	delay, err := raceEgress(context.Background(), targets, func(_ context.Context, target LegEgressTarget) (time.Duration, error) {
		if target.Host == "origin-a" {
			return time.Second, errors.New("origin unavailable")
		}
		return 200 * time.Millisecond, nil
	})
	if err != nil || delay != 200*time.Millisecond {
		t.Fatalf("healthy alternative rejected: delay=%s err=%v", delay, err)
	}
}

// Quality/what: no successful origin means no invented external connectivity.
func TestPerformanceEgressRequiresSuccessAndHonorsCancellation(t *testing.T) {
	// Why: retries and hedging must retain complete failure and shutdown signals.
	targets := []LegEgressTarget{{Host: "origin-a"}, {Host: "origin-b"}}
	_, err := raceEgress(context.Background(), targets, func(context.Context, LegEgressTarget) (time.Duration, error) {
		return time.Second, errors.New("failed")
	})
	if err == nil || !strings.Contains(err.Error(), "origin-a") || !strings.Contains(err.Error(), "origin-b") {
		t.Fatalf("failure details lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = raceEgress(ctx, targets, func(context.Context, LegEgressTarget) (time.Duration, error) { return time.Millisecond, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled measurement qualified a leg")
	}
}

// Quality/what: a slow alternate request cannot delay a valid fast response.
func TestPerformanceEgressWinnerCancelsTheOtherProbe(t *testing.T) {
	// Why: two endpoints must improve coverage without doubling probe latency/load.
	started, canceled := make(chan struct{}), make(chan struct{})
	targets := []LegEgressTarget{{Host: "slow"}, {Host: "fast"}}
	_, err := raceEgress(context.Background(), targets, func(ctx context.Context, target LegEgressTarget) (time.Duration, error) {
		if target.Host == "slow" {
			close(started)
			<-ctx.Done()
			close(canceled)
			return 0, ctx.Err()
		}
		<-started
		return time.Millisecond, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("losing request kept running")
	}
}
