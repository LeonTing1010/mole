package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Safety/what: busy traffic or absent counters cannot qualify a comparison.
func TestAllocationTrafficGuardRejectsBusyResetAndMissingPayload(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name          string
		before, after allocationTraffic
		payload       int64
		duration      time.Duration
		valid         bool
	}{
		{"idle", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 200, Up: 200}, 0, 2 * time.Second, true},
		{"busy user", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 4 * mib, Up: 100}, 0, 2 * time.Second, false},
		{"probe only", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 32*mib + 100, Up: 100}, 32 * mib, 3 * time.Second, true},
		{"missing bytes", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 31 * mib, Up: 100}, 32 * mib, 3 * time.Second, false},
		{"counter reset", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 10, Up: 100}, 0, 2 * time.Second, false},
		{"upload saturation", allocationTraffic{Down: 100, Up: 100}, allocationTraffic{Down: 100, Up: 4 * mib}, 0, 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok := allocationTrafficError(tc.before, tc.after, tc.payload, tc.duration) == nil; ok != tc.valid {
				t.Fatalf("clean=%t", ok)
			}
		})
	}
}

// Quality/what: the loaded-gateway guard fails closed without diagnosing Wi-Fi.
func TestAllocationLatencyGuardRejectsMissingOrLoadedDelay(t *testing.T) {
	for _, tc := range []struct {
		idle, loaded time.Duration
		valid        bool
	}{
		{4 * time.Millisecond, 15 * time.Millisecond, true},
		{4 * time.Millisecond, 180 * time.Millisecond, false},
		{0, 10 * time.Millisecond, false},
		{50 * time.Millisecond, 51 * time.Millisecond, false},
		{4 * time.Millisecond, 0, false},
	} {
		if got := allocationLatencyClean(tc.idle, tc.loaded); got != tc.valid {
			t.Fatalf("idle=%s loaded=%s clean=%t", tc.idle, tc.loaded, got)
		}
	}
}

// Safety/what-x-what: a loaded-gateway spike cancels the in-flight download.
func TestAllocationGuardCancelsLoadedProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopped := make(chan struct{})
	_, err := guardedAllocationMeasure(ctx, 4*time.Millisecond, 5*time.Millisecond,
		func(context.Context) (time.Duration, error) { return 200 * time.Millisecond, nil },
		func() error { return nil },
		func(ctx context.Context) (allocationObservation, error) {
			<-ctx.Done()
			close(stopped)
			return allocationObservation{}, ctx.Err()
		},
	)
	if err == nil || !strings.Contains(err.Error(), "200ms") || !strings.Contains(err.Error(), "75ms") {
		t.Fatalf("missing loaded gateway evidence: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("in-flight sample was not canceled")
	}
}

// Safety/what: missing a loaded observation cannot qualify a short download.
func TestAllocationGuardRequiresAProbeDuringDownload(t *testing.T) {
	_, err := guardedAllocationMeasure(context.Background(), 4*time.Millisecond, time.Second,
		func(context.Context) (time.Duration, error) { t.Fatal("unexpected gateway probe"); return 0, nil },
		func() error { return nil },
		func(context.Context) (allocationObservation, error) {
			return allocationObservation{Mbps: 100, Duration: 2 * time.Second}, nil
		},
	)
	if err == nil {
		t.Fatal("no loaded latency evidence accepted")
	}
}

// Safety/what: a gateway ping after the download cannot be counted as loaded.
func TestAllocationGuardRejectsGatewayProbeAfterDownloadFinished(t *testing.T) {
	var probes atomic.Int32
	_, err := guardedAllocationMeasure(context.Background(), 4*time.Millisecond, 5*time.Millisecond,
		func(context.Context) (time.Duration, error) { probes.Add(1); return 4 * time.Millisecond, nil },
		func() error { time.Sleep(20 * time.Millisecond); return nil },
		func(context.Context) (allocationObservation, error) {
			time.Sleep(10 * time.Millisecond)
			return allocationObservation{Mbps: 100, Duration: 2 * time.Second}, nil
		},
	)
	if err == nil || probes.Load() != 0 {
		t.Fatalf("post-download gateway ping accepted: probes=%d err=%v", probes.Load(), err)
	}
}

// Quality/what-x-what: synthetic bucket shares require compatible observed flows.
func TestAllocationFlowGuardRejectsUnrepresentativeAndSparseMixes(t *testing.T) {
	// Why: eight equal-sized probes do not reproduce a skewed source-port census.
	skew := [8]int{4, 2, 1, 5, 6, 6, 1, 1}
	balanced := [8]int{3, 3, 3, 3, 3, 3, 3, 3}
	for _, tc := range []struct {
		name               string
		counts             [8]int
		current, candidate int
		valid              bool
	}{
		{"observed skew", skew, 0, 1, false},
		{"balanced sample", balanced, 0, 1, true},
		{"too few flows", [8]int{1, 1, 1, 1, 1, 1, 1, 1}, 0, 1, false},
		{"pure endpoints", [8]int{}, 0, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := allocationFlowRepresentative(tc.counts, tc.current, tc.candidate); got != tc.valid {
				t.Fatalf("representative=%t", got)
			}
		})
	}
}

// Quality/what: counters and real selector tags must be read without destinations.
func TestAllocationTotalsRequireCountersAndCountEachFlowOnce(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"valid", `{"downloadTotal":25,"uploadTotal":4,"connections":[{"chains":["proxy-reality","flow-7","flow-7"]},{"chains":["direct"]},{"chains":["flow-1"]}]}`, false},
		{"missing", `{"connections":[]}`, true},
		{"negative", `{"downloadTotal":-1,"uploadTotal":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
			defer api.Close()
			c := NewClashClient(api.Listener.Addr().String())
			got, err := c.allocationTotals(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if !tc.wantErr && (got.Down != 25 || got.Up != 4 || got.Flows[7] != 1 || got.Flows[1] != 1) {
				t.Fatalf("unexpected counts %+v", got)
			}
		})
	}
}

// Quality/what: rejected traffic evidence must identify the failed condition.
func TestAllocationTrafficGuardExplainsCounterAndBackgroundFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after allocationTraffic
		payload       int64
		want          string
	}{
		{"reset", allocationTraffic{Down: 10}, allocationTraffic{Down: 1}, 0, "reset"},
		{"missing", allocationTraffic{Down: 0}, allocationTraffic{Down: 1}, 100, "not accounted"},
		{"busy", allocationTraffic{}, allocationTraffic{Down: 8 << 20}, 0, "other counted download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := allocationTrafficError(tc.before, tc.after, tc.payload, 2*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reason=%v", err)
			}
		})
	}
}
