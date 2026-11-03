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

// Quality/what-x-what: fast protection must not consume minute-scale evidence.
func TestUDPPressureSnapshotPreservesSchedulerSightings(t *testing.T) {
	// Why: both controllers need the same overflow evidence without double counting.
	var f floodSensor
	io.WriteString(&f, enobufsLine)
	a := f.snapshot()
	if a.sequence != 1 || a.last.IsZero() {
		t.Fatal("no independent pressure evidence")
	}
	if n, _ := f.take(); n != 1 {
		t.Fatal("fast snapshot stole self-healing evidence")
	}
	if b := f.snapshot(); b != a {
		t.Fatal("consuming minute count destroyed cumulative snapshot")
	}
}

// Quality/what-x-what: protection must reach selectors before adding probe load.
func TestUDPPressureControllerDrainsNewFlowsAndReportsActualRung(t *testing.T) {
	// Why: changing only bookkeeping leaves the overloaded UDP socket in use.
	// The ceiling cap (capDown) is the real pressure protection; draining all
	// UDP lanes to 0 as a permanent state was removed because it created a
	// cascade during cooling windows (all traffic → TCP → TCP probe slow →
	// hy2 leg stays unhealthy → Brutal never probes). The temporary pre-probe
	// drain still happens before probes, but observe() restores proper lane
	// allocation from fresh probe data afterward.
	t.Setenv("HOME", t.TempDir())
	if err := utils.WriteLeg(utils.LegBoth); err != nil {
		t.Fatal(err)
	}
	selected := map[string]string{ProxySelectorTag: RealityLegTag, Hy2ProbeSelectorTag: BandwidthRungTag(150)}
	for i := 0; i < BalanceLanes; i++ {
		selected[BalanceLaneTag(i)] = Hy2ProbeSelectorTag
	}
	c := NewClashClient("control.invalid")
	c.http.Transport = probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": selected[tag]})
		} else {
			var req struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			selected[tag] = req.Name
			w.WriteHeader(204)
		}
		return w.Result(), nil
	})
	now := time.Now()
	s := &Supervisor{
		schedule: BandwidthSchedule{OffUp: 100, OffDown: 400}, clash: c,
		state:              &utils.State{BandwidthDownMbps: 150, Balance: &utils.BalanceStatus{Supported: true, Enabled: true}},
		pressureSnapshotFn: func() floodSnapshot { return floodSnapshot{1, now} },
		legProbeFn: func(int, int) (time.Duration, error) {
			// Both concurrent probes read only after all selector writes are complete.
			for i := 0; i < BalanceLanes; i++ {
				if selected[BalanceLaneTag(i)] != RealityLegTag {
					return 0, fmt.Errorf("UDP not drained before probes")
				}
			}
			return cleanLeg.delay, nil
		},
	}
	p := balancePolicy{reality: balanceLeg{healthy: true}, hy2: balanceLeg{healthy: true}, hy2Lanes: BalanceLanes}
	for i := 0; i < 4; i++ {
		s.balanceOnce(&p)
	}
	b := s.state.Balance
	if selected[Hy2ProbeSelectorTag] != BandwidthRungTag(100) || b.Hy2DownMbps != 100 || b.Hy2UpMbps != 25 {
		t.Fatalf("actual guarded ceiling not selected/published: %+v", b)
	}
	// Lanes are NOT forced to 0 during cooling — observe() computes the
	// correct allocation from probe health. The real pressure protection is
	// the ceiling cap + cooldown window, not permanently disabling UDP.
	if b.RealityHealthy != true || b.Hy2Healthy != true || b.UDPCooldownUntil.IsZero() || b.RealityError != "" || b.Hy2Error != "" {
		t.Fatalf("pressure protection not published: %+v", b)
	}
	if p.pressure.capDown != 100 {
		t.Fatal("same event repeatedly lowered the ceiling")
	}
	// The guard must not remove the only working transport.
	p.pressure.capDown = 1
	p.reality.healthy, p.hy2.healthy, p.hy2Lanes = false, true, BalanceLanes
	s.legProbeFn = func(port, _ int) (time.Duration, error) {
		if port == BrutalKeeperPort {
			return brokenLeg.delay, brokenLeg.err
		}
		return cleanLeg.delay, nil
	}
	s.balanceOnce(&p)
	if s.state.Balance.Hy2Lanes != BalanceLanes {
		t.Fatal("cooldown withdrew the only working leg")
	}
	// No-link time must not count as clean recovery time.
	s.state.LinkBad = true
	p.pressure.cleanSince = time.Now().Add(-time.Hour)
	s.balanceOnce(&p)
	if !p.pressure.cleanSince.IsZero() {
		t.Fatal("local outage counted as clean UDP health")
	}
	s.state.LinkBad = false
	if err := utils.WriteLeg(utils.LegReality); err != nil {
		t.Fatal(err)
	}
	s.balanceOnce(&p)
	if p.pressure.capDown != 0 || s.state.Balance.Enabled {
		t.Fatal("manual mode did not release dual guard")
	}
}

// Quality/what: one burst lowers once; sustained pressure can pass the old floor.
func TestUDPPressureBoundsBurstAndRespondsToPersistentOverflow(t *testing.T) {
	// Why: many closed flows can repeat one socket error; they are not many capacity measurements.
	s := BandwidthSchedule{OffDown: 400, OffUp: 100}
	now := time.Unix(1000, 0)
	var p udpPressure
	if got := p.target(floodSnapshot{1, now}, now, 400, 400, s, true); got != 300 || !p.cooling(now) {
		t.Fatalf("first pressure target=%d cooling=%t", got, p.cooling(now))
	}
	if got := p.target(floodSnapshot{100, now.Add(time.Second)}, now.Add(time.Second), 300, 400, s, true); got != 300 {
		t.Fatalf("one burst descended repeatedly: %d", got)
	}
	for i, want := range []int{200, 150, 100, 80, 65, 50, 30, 20, 12, 8, 5, 3, 2, 1, 1} {
		now = now.Add(time.Minute)
		got := p.target(floodSnapshot{uint64(101 + i), now}, now, p.capDown, 400, s, true)
		if got != want {
			t.Fatalf("persistent overflow step %d: %d want %d", i, got, want)
		}
	}
}

// Quality/what: old log events cannot throttle a newly enabled transport.
func TestUDPPressureIgnoresStaleAndRestoresOnlyAfterCleanHealth(t *testing.T) {
	// Why: restore capacity cautiously, without letting an outage count as a clean recovery.
	s := BandwidthSchedule{OffDown: 400, OffUp: 100}
	now := time.Unix(1000, 0)
	var p udpPressure
	if got := p.target(floodSnapshot{1, now.Add(-2 * time.Minute)}, now, 400, 400, s, true); got != 400 {
		t.Fatal("stale evidence lowered capacity")
	}
	snap := floodSnapshot{2, now}
	p.target(snap, now, 150, 400, s, true)
	if got := p.target(snap, now.Add(14*time.Minute), 100, 400, s, true); got != 100 {
		t.Fatal("premature recovery")
	}
	p.target(snap, now.Add(15*time.Minute), 100, 400, s, false)
	p.target(snap, now.Add(16*time.Minute), 100, 400, s, true)
	if got := p.target(snap, now.Add(30*time.Minute), 100, 400, s, true); got != 100 {
		t.Fatal("outage counted toward clean interval")
	}
	if got := p.target(snap, now.Add(31*time.Minute), 100, 400, s, true); got != 150 {
		t.Fatalf("clean recovery target=%d want 150", got)
	}
	if got := p.target(snap, now.Add(32*time.Minute), 150, 80, s, true); got != 80 {
		t.Fatal("pressure guard overrode lower operator/scheduler ceiling")
	}
}
