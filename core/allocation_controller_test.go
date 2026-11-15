package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func allocationTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := utils.WriteLeg(utils.LegBoth); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("sampling changed live selectors")
		}
		if !strings.HasSuffix(r.URL.Path, Hy2ProbeSelectorTag) {
			t.Error(r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"now": BandwidthRungTag(100)})
	}))
	t.Cleanup(api.Close)
	return &Supervisor{state: &utils.State{BandwidthDownMbps: 100, RealityRateMbps: 500}, clash: NewClashClient(api.Listener.Addr().String()), allocationSampleFn: func(share int) (float64, error) {
		if share == 2 {
			return 140, nil
		}
		return 100, nil
	}}
}
func qualifiedAllocationPolicy() balancePolicy {
	return balancePolicy{reality: balanceLeg{healthy: true}, hy2: balanceLeg{healthy: true}}
}

// Quality/what-x-what: complete sweeps may steer new flows, but cannot run every tick.
func TestAllocationControllerBoundsSamplingAndCommitsRepeatedWinner(t *testing.T) {
	// Why: background optimization must have a bounded data budget and avoid noise.
	s := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	calls := 0
	sample := s.allocationSampleFn
	s.allocationSampleFn = func(share int) (float64, error) { calls++; return sample(share) }
	now := time.Now()
	s.sampleAllocation(&p, now)
	if calls != 9 || p.search.preferred != 0 || p.sampledAt.IsZero() {
		t.Fatalf("first scan: calls=%d policy=%+v", calls, p)
	}
	s.sampleAllocation(&p, now.Add(time.Minute))
	if calls != 9 {
		t.Fatal("scan exceeded cooldown budget")
	}
	s.sampleAllocation(&p, now.Add(10*time.Minute))
	if calls != 18 || p.search.preferred != 2 {
		t.Fatalf("repeated winner: calls=%d share=%d", calls, p.search.preferred)
	}
}

// Safety/what-x-what: evidence cannot cross a mode, link, pacing or pressure change.
func TestAllocationControllerRejectsChangedEnvironmentAndFailures(t *testing.T) {
	for _, change := range []string{"mode", "link", "rate", "rung", "overflow", "payload"} {
		t.Run(change, func(t *testing.T) {
			s := allocationTestSupervisor(t)
			p := qualifiedAllocationPolicy()
			s.sampleAllocation(&p, time.Now()) // First potential win.
			original := s.allocationSampleFn
			seq := uint64(0)
			s.pressureSnapshotFn = func() floodSnapshot { return floodSnapshot{sequence: seq} }
			s.allocationSampleFn = func(share int) (float64, error) {
				switch change {
				case "mode":
					utils.WriteLeg(utils.LegReality)
				case "link":
					s.state.LinkBad = true
				case "rate":
					s.state.RealityRateMbps++
				case "rung":
					s.clash.http.Transport = bulkSpeedTransport(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"now":"proxy-bw-200"}`))}, nil
					})
				case "overflow":
					seq++
				case "payload":
					return 0, fmt.Errorf("partial payload")
				}
				return original(share)
			}
			s.sampleAllocation(&p, time.Now().Add(10*time.Minute))
			if p.search.preferred != 0 || p.search.wins != 0 || p.sampleError == "" {
				t.Fatalf("changed %s accepted: %+v", change, p)
			}
		})
	}
}

// Quality/what: no download probes on a failed link, manual pin or unhealthy leg.
func TestAllocationControllerSkipsIneligibleSampling(t *testing.T) {
	for _, reason := range []string{"link", "pin", "recovering", "pressure", "slow"} {
		t.Run(reason, func(t *testing.T) {
			s := allocationTestSupervisor(t)
			p := qualifiedAllocationPolicy()
			switch reason {
			case "link":
				s.state.LinkBad = true
			case "pin":
				utils.ClearLeg()
			case "recovering":
				p.hy2.healthy = false
			case "pressure":
				p.pressure.coolUntil = time.Now().Add(time.Hour)
			case "slow":
				p.hy2.slow = 1
			}
			s.allocationSampleFn = func(int) (float64, error) { t.Fatal("ineligible sample"); return 0, nil }
			s.sampleAllocation(&p, time.Now())
		})
	}
}

// Quality/what: repeated wins must describe the same pacing environment.
func TestAllocationControllerDoesNotCombineWinsAcrossRateProfiles(t *testing.T) {
	// Why: a change between scans invalidates the previous confirmation vote too.
	s := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	now := time.Now()
	s.sampleAllocation(&p, now)
	s.state.RealityRateMbps = 200
	s.sampleAllocation(&p, now.Add(allocationSampleEvery))
	if p.search.preferred != 0 {
		t.Fatal("wins from different pacing profiles combined")
	}
	s.sampleAllocation(&p, now.Add(2*allocationSampleEvery))
	if p.search.preferred != 2 {
		t.Fatal("stable new profile did not qualify")
	}
}
