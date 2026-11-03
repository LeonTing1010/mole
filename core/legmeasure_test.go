package core

import (
	"errors"
	"testing"
	"time"
)

// Quality/what: each transport owns its failure, including a fully dark primary.
func TestPairedLegFailuresDoNotHideHealthyStandby(t *testing.T) {
	// Why: a primary timeout is evidence for failover, not a failed comparison.
	for _, badPort := range []int{BrutalKeeperPort, LegGatePort, 0} {
		counts := map[int]int{}
		hy2, reality, err := measureLegs(func(port int) (time.Duration, error) {
			counts[port]++
			if port == badPort || badPort == 0 {
				return 5 * time.Second, errors.New("timeout")
			}
			return 200 * time.Millisecond, nil
		}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if counts[BrutalKeeperPort] != 3 || counts[LegGatePort] != 3 {
			t.Fatalf("both legs need three samples: %v", counts)
		}
		if got := legComparison(hy2, reality); got != (badPort == BrutalKeeperPort) {
			t.Fatalf("bad port %d: switch=%v", badPort, got)
		}
	}
}

// Quality/what: a single good moment cannot qualify a recovered primary.
func TestRecoveryRequiresThreeCleanSpacedDials(t *testing.T) {
	// Why: intermittent faults pass one probe and strand traffic after handback.
	for _, bad := range []int{-1, 0, 1, 2} {
		calls, waits := 0, 0
		err := verifyLegRecovery(func() (time.Duration, error) {
			i := calls
			calls++
			if i == bad {
				return 5 * time.Second, errors.New("timeout")
			}
			return 200 * time.Millisecond, nil
		}, func() error { waits++; return nil })
		if (err == nil) != (bad == -1) {
			t.Fatalf("bad sample %d: err=%v", bad, err)
		}
		if bad == -1 && (calls != 3 || waits != 2) {
			t.Fatalf("calls=%d waits=%d", calls, waits)
		}
	}
	if err := verifyLegRecovery(func() (time.Duration, error) { return 2 * time.Second, nil }, func() error { return nil }); err == nil {
		t.Fatal("slow primary qualified for handback")
	}
}

func TestLegMeasurementsStopOnCancellation(t *testing.T) {
	// Quality/what. Why: shutdown must not resume traffic on a partly tested leg.
	stopped := errors.New("stopped")
	_, _, err := measureLegs(func(int) (time.Duration, error) { return time.Millisecond, nil }, func() error { return stopped })
	if !errors.Is(err, stopped) {
		t.Fatal("comparison ignored cancellation")
	}
	err = verifyLegRecovery(func() (time.Duration, error) { return time.Millisecond, nil }, func() error { return stopped })
	if !errors.Is(err, stopped) {
		t.Fatal("recovery ignored cancellation")
	}
}
