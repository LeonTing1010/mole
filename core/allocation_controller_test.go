package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func allocationTestSupervisor(t *testing.T) (*Supervisor, map[string]string, *sync.Mutex) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := utils.WriteLeg(utils.LegBoth); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	selected := map[string]string{Hy2ProbeSelectorTag: BandwidthRungTag(100), ProxySelectorTag: RealityLegTag}
	for i := 0; i < BalanceLanes; i++ {
		selected[BalanceLaneTag(i)] = RealityLegTag
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/connections" {
			counts := [8]int{3, 3, 3, 3, 3, 3, 3, 3}
			if selected["#skew"] == "1" {
				counts = [8]int{4, 2, 1, 5, 6, 6, 1, 1}
			}
			var connections []map[string]any
			for i, n := range counts {
				for j := 0; j < n; j++ {
					connections = append(connections, map[string]any{"chains": []string{BalanceLaneTag(i)}})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"downloadTotal": 0, "uploadTotal": 0, "connections": connections})
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if selected["#fail"] == tag {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": selected[tag]})
			return
		}
		var d struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		selected[tag] = d.Name
		w.WriteHeader(204)
	}))
	t.Cleanup(api.Close)
	s := &Supervisor{state: &utils.State{BandwidthDownMbps: 100, RealityRateMbps: 500, LinkGateway: "127.0.0.1"}, clash: NewClashClient(api.Listener.Addr().String()), pressureSnapshotFn: func() floodSnapshot { return floodSnapshot{} }}
	s.allocationUplinkFn = func() (string, error) { return "test-uplink", nil }
	s.allocationRunFn = func(_ context.Context, current, candidate int, edges bool, _ func() error) (allocationTrial, error) {
		return allocationTestTrial(current, candidate, edges), nil
	}
	return s, selected, &mu
}
func allocationTestTrial(current, candidate int, edges bool) allocationTrial {
	o := allocationObservation{Mbps: 100, Duration: 3 * time.Second, TotalBytes: 64 << 20}
	b := o
	b.Mbps = 140
	t := allocationTrial{Current: current, Candidate: candidate, CandidateOnEdges: edges, Samples: [3]allocationObservation{o, b, o}}
	if edges {
		t.Samples = [3]allocationObservation{b, o, b}
	}
	return t
}
func qualifiedAllocationPolicy() balancePolicy {
	return balancePolicy{reality: balanceLeg{healthy: true}, hy2: balanceLeg{healthy: true}}
}
func awaitAllocation(t *testing.T, p *balancePolicy) {
	t.Helper()
	if p.sampleJob == nil {
		t.Fatal("trial not launched")
	}
	select {
	case result := <-p.sampleJob:
		p.sampleJob <- result
	case <-time.After(2 * time.Second):
		t.Fatal("trial did not finish")
	}
}

// Quality/what-x-what: only repeated paired wins may change an applied allocation.
func TestAllocationControllerBoundsSamplingAndCommitsRepeatedWinner(t *testing.T) {
	s, _, _ := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	now := time.Now()
	for i := 0; i < 3; i++ {
		at := now.Add(time.Duration(i) * allocationSampleEvery)
		s.sampleAllocation(&p, at, true)
		awaitAllocation(t, &p)
		s.sampleAllocation(&p, at.Add(time.Second), false)
		if i < 2 && p.search.preferred != 0 {
			t.Fatal("switched before three comparisons")
		}
		if i < 2 {
			s.sampleAllocation(&p, at.Add(time.Minute), true)
			if p.sampleJob != nil {
				t.Fatal("cooldown failed")
			}
		}
	}
	if p.search.preferred != 1 || p.sampleEvidence == nil || !p.sampleEvidence.Qualified {
		t.Fatalf("no confirmed candidate: %+v", p)
	}
}

// Safety/what-x-what: a trial cannot commit across changes or partial samples.
func TestAllocationControllerRejectsChangedEnvironmentAndFailures(t *testing.T) {
	for _, cause := range []string{"mode", "link", "rate", "rung", "overflow", "payload"} {
		t.Run(cause, func(t *testing.T) {
			s, selected, mu := allocationTestSupervisor(t)
			p := qualifiedAllocationPolicy()
			p.search.pending = 1
			p.search.wins = 2
			var seq atomic.Uint64
			s.pressureSnapshotFn = func() floodSnapshot { return floodSnapshot{sequence: seq.Load()} }
			s.allocationRunFn = func(_ context.Context, current, candidate int, edges bool, _ func() error) (allocationTrial, error) {
				switch cause {
				case "mode":
					utils.WriteLeg(utils.LegReality)
				case "link":
					s.stateMu.Lock()
					s.state.LinkBad = true
					s.stateMu.Unlock()
				case "rate":
					s.stateMu.Lock()
					s.state.RealityRateMbps++
					s.stateMu.Unlock()
				case "rung":
					mu.Lock()
					selected[Hy2ProbeSelectorTag] = BandwidthRungTag(200)
					mu.Unlock()
				case "overflow":
					seq.Add(1)
				case "payload":
					return allocationTrial{}, fmt.Errorf("partial payload")
				}
				return allocationTestTrial(current, candidate, edges), nil
			}
			now := time.Now()
			s.sampleAllocation(&p, now, true)
			awaitAllocation(t, &p)
			s.sampleAllocation(&p, now.Add(time.Second), false)
			if p.search.preferred != 0 || p.search.wins != 0 {
				t.Fatalf("unsafe comparison accepted: %+v", p.search)
			}
		})
	}
}

// Quality/what: ineligible health, user pins and pressure do not start downloads.
func TestAllocationControllerSkipsIneligibleSampling(t *testing.T) {
	for _, reason := range []string{"link", "pin", "recovering", "pressure", "slow", "reality_error", "hy2_error"} {
		t.Run(reason, func(t *testing.T) {
			s, _, _ := allocationTestSupervisor(t)
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
			case "reality_error":
				p.reality.bad = 1
			case "hy2_error":
				p.hy2.bad = 1
			}
			s.allocationRunFn = func(context.Context, int, int, bool, func() error) (allocationTrial, error) {
				t.Fatal("ineligible sample")
				return allocationTrial{}, nil
			}
			s.sampleAllocation(&p, time.Now(), true)
			if p.sampleJob != nil {
				t.Fatal("ineligible trial started")
			}
		})
	}
}

// Quality/what: earlier votes cannot cross a rate profile change.
func TestAllocationControllerDoesNotCombineWinsAcrossRateProfiles(t *testing.T) {
	s, _, _ := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	now := time.Now()
	s.sampleAllocation(&p, now, true)
	awaitAllocation(t, &p)
	s.sampleAllocation(&p, now.Add(time.Second), false)
	s.state.RealityRateMbps = 200
	s.sampleAllocation(&p, now.Add(allocationSampleEvery), true)
	awaitAllocation(t, &p)
	s.sampleAllocation(&p, now.Add(allocationSampleEvery+time.Second), false)
	if p.search.preferred != 0 || p.search.wins != 1 {
		t.Fatalf("profile change combined votes: %+v", p.search)
	}
}

// Safety/what-x-what: long background trials must not delay leg withdrawal.
func TestAllocationHealthControlContinuesDuringSlowTrial(t *testing.T) {
	s, selected, mu := allocationTestSupervisor(t)
	s.schedule = BandwidthSchedule{OffDown: 100}
	s.state.Balance = &utils.BalanceStatus{Supported: true}
	var broken atomic.Bool
	s.legProbeFn = func(port, _ int) (time.Duration, error) {
		if port == LegGatePort && broken.Load() {
			return brokenLeg.delay, brokenLeg.err
		}
		return cleanLeg.delay, nil
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	s.allocationRunFn = func(ctx context.Context, current, candidate int, edges bool, _ func() error) (allocationTrial, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return allocationTrial{}, ctx.Err()
	}
	p := qualifiedAllocationPolicy()
	p.search.preferred = 2
	// Existing applied state matches the selected two-lane allocation.
	mu.Lock()
	for i := 0; i < BalanceLanes; i++ {
		if (i+1)*2/BalanceLanes > i*2/BalanceLanes {
			selected[BalanceLaneTag(i)] = Hy2ProbeSelectorTag
		}
	}
	mu.Unlock()
	s.balanceOnce(&p)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("trial did not start")
	}
	broken.Store(true)
	s.balanceOnce(&p)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("trial not canceled at first leg error")
	}
	if !s.state.Balance.Hy2Healthy || s.state.Balance.Hy2Lanes != 2 {
		t.Fatal("one probe error must not bypass leg withdrawal hysteresis")
	}
	done := make(chan struct{})
	go func() {
		for i := 1; i < legErrorThreshold; i++ {
			s.balanceOnce(&p)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("health reconciliation blocked by trial")
	}
	if s.state.Balance.Hy2Lanes != 0 || s.state.Balance.Hy2Healthy {
		t.Fatalf("failed leg retained allocation: %+v", s.state.Balance)
	}
}

// Safety/what-x-what: loss of selector control invalidates in-flight evidence.
func TestAllocationControlErrorCancelsSampling(t *testing.T) {
	for _, failedTag := range []string{Hy2ProbeSelectorTag, ProxySelectorTag} {
		t.Run(failedTag, func(t *testing.T) {
			s, selected, mu := allocationTestSupervisor(t)
			s.schedule = BandwidthSchedule{OffDown: 100}
			s.state.Balance = &utils.BalanceStatus{Supported: true}
			s.legProbeFn = func(int, int) (time.Duration, error) { return cleanLeg.delay, nil }
			started := make(chan struct{})
			stopped := make(chan struct{})
			s.allocationRunFn = func(ctx context.Context, _, _ int, _ bool, _ func() error) (allocationTrial, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return allocationTrial{}, ctx.Err()
			}
			p := qualifiedAllocationPolicy()
			s.balanceOnce(&p)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("trial did not start")
			}
			p.search.pending, p.search.wins = p.candidate, 2
			mu.Lock()
			selected["#fail"] = failedTag
			mu.Unlock()
			s.balanceOnce(&p)
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("selector error did not cancel trial")
			}
			if p.sampleJob != nil || p.search.wins != 0 || s.state.Balance.Sampling {
				t.Fatalf("selector error retained sampling or evidence: %+v", s.state.Balance)
			}
		})
	}
}

// Quality/what: an interrupted trial must not remain visible as running.
func TestAllocationLocalLinkFailureClearsSamplingStatus(t *testing.T) {
	s, _, _ := allocationTestSupervisor(t)
	s.schedule = BandwidthSchedule{OffDown: 100}
	s.state.Balance = &utils.BalanceStatus{Supported: true}
	s.legProbeFn = func(int, int) (time.Duration, error) { return cleanLeg.delay, nil }
	started := make(chan struct{})
	stopped := make(chan struct{})
	s.allocationRunFn = func(ctx context.Context, _, _ int, _ bool, _ func() error) (allocationTrial, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return allocationTrial{}, ctx.Err()
	}
	p := qualifiedAllocationPolicy()
	s.balanceOnce(&p)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("trial did not start")
	}
	s.stateMu.Lock()
	s.state.LinkBad = true
	s.stateMu.Unlock()
	s.balanceOnce(&p)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("link failure did not cancel trial")
	}
	if s.state.Balance.Sampling || s.state.Balance.SampleError == "" {
		t.Fatalf("interrupted trial still displayed as running: %+v", s.state.Balance)
	}
}

// Safety/what-x-what: stale performance preferences cannot survive a changed path.
func TestAllocationRevertsPreferenceWhenMeasuredEnvironmentChanges(t *testing.T) {
	s, selected, mu := allocationTestSupervisor(t)
	s.schedule = BandwidthSchedule{OffDown: 100}
	s.state.Balance = &utils.BalanceStatus{Supported: true}
	s.legProbeFn = func(int, int) (time.Duration, error) { return cleanLeg.delay, nil }
	p := qualifiedAllocationPolicy()
	p.search.preferred = 2
	env, err := s.allocationEnv()
	if err != nil {
		t.Fatal(err)
	}
	p.sampleEnvironment = env
	mu.Lock()
	for i := 0; i < BalanceLanes; i++ {
		if (i+1)*2/BalanceLanes > i*2/BalanceLanes {
			selected[BalanceLaneTag(i)] = Hy2ProbeSelectorTag
		}
	}
	mu.Unlock()
	s.state.RealityRateMbps = 300
	s.balanceOnce(&p)
	if s.state.Balance.Hy2Lanes != 0 || p.search.preferred != 0 {
		t.Fatalf("stale preference retained: %+v", s.state.Balance)
	}
}

// Safety/what-x-what: a finished trial cannot promote a now-skewed workload.
func TestAllocationRechecksFlowDistributionBeforePromotion(t *testing.T) {
	s, selected, mu := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	p.search.pending = 1
	p.search.wins = 2
	p.search.gains = [3]float64{1.4, 1.4, 0}
	env, err := s.allocationEnv()
	if err != nil {
		t.Fatal(err)
	}
	p.sampleEnvironment = env
	now := time.Now()
	s.sampleAllocation(&p, now, true)
	awaitAllocation(t, &p)
	mu.Lock()
	selected["#skew"] = "1"
	mu.Unlock()
	s.sampleAllocation(&p, now.Add(time.Second), false)
	if p.search.preferred != 0 || p.search.wins != 0 {
		t.Fatalf("skewed actual flow was accepted: %+v", p.search)
	}
}

// Quality/what: a skewed candidate does not block exploration forever.
func TestAllocationAdvancesPastUnrepresentativeCandidate(t *testing.T) {
	s, _, _ := allocationTestSupervisor(t)
	p := qualifiedAllocationPolicy()
	now := time.Now()
	s.allocationRunFn = func(context.Context, int, int, bool, func() error) (allocationTrial, error) {
		return allocationTrial{}, errAllocationFlowSkew
	}
	s.sampleAllocation(&p, now, true)
	awaitAllocation(t, &p)
	s.sampleAllocation(&p, now.Add(time.Second), false)
	if p.candidate != 2 || p.search.preferred != 0 || p.search.wins != 0 {
		t.Fatalf("bad candidate transition: %+v", p)
	}
}
