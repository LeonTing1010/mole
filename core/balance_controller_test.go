package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Quality/what-x-what: qualified dual traffic remains controllable by manual mode.
func TestBalanceControllerQualifiesBothWithdrawsFailureAndRestoresSingleMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := utils.WriteLeg(utils.LegBoth); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now := map[string]string{ProxySelectorTag: RealityLegTag, Hy2ProbeSelectorTag: "proxy-bw-100"}
	for i := 0; i < BalanceLanes; i++ {
		now[BalanceLaneTag(i)] = ProxySelectorTag
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": now[tag]})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		now[tag] = req.Name
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	broken := false
	s := &Supervisor{
		hasReality: true, verifyLegEgress: true, schedule: BandwidthSchedule{OffDown: 100},
		state: &utils.State{BandwidthDownMbps: 100, Balance: &utils.BalanceStatus{Supported: true}},
		clash: NewClashClient(api.Listener.Addr().String()),
		legProbeFn: func(port, _ int) (time.Duration, error) {
			if broken && port == LegGatePort {
				return brokenLeg.delay, brokenLeg.err
			}
			return cleanLeg.delay, nil
		},
	}
	var policy balancePolicy
	for i := 0; i < balanceRecoverSamples; i++ {
		s.balanceOnce(&policy)
	}
	if !s.state.Balance.Enabled || s.state.Balance.Hy2Lanes < 1 {
		t.Fatalf("dual mode did not ramp: %+v", s.state.Balance)
	}
	broken = true
	s.balanceOnce(&policy) // 1 broken → bad=1, still healthy
	s.balanceOnce(&policy) // 2 broken → bad=2, still healthy
	s.balanceOnce(&policy) // 3 broken → bad=3, unhealthy → withdraw
	if s.state.Balance.Hy2Lanes != 0 {
		t.Fatal("broken UDP retained new flows")
	}
	// Nothing may move the ladder's selector once observe() has withdrawn the
	// lanes: it shares Hy2ProbeSelectorTag with real traffic (see spec.md F3).
	if udp, err := s.clash.Selection(Hy2ProbeSelectorTag); err != nil || udp != "proxy-bw-100" {
		t.Fatalf("dual mode must leave the selector alone after withdrawal: got %s err %v", udp, err)
	}
	if err := utils.WriteLeg(utils.LegHy2); err != nil {
		t.Fatal(err)
	}
	s.balanceOnce(&policy)
	if s.state.Balance.Enabled {
		t.Fatal("dual mode overrode manual pin")
	}
	for i := 0; i < BalanceLanes; i++ {
		got, err := s.clash.Selection(BalanceLaneTag(i))
		if err != nil || got != ProxySelectorTag {
			t.Fatalf("lane %d ignored main selector: %q %v", i, got, err)
		}
	}
}

// Quality/what: failed control writes cannot accumulate an unseen ramp.
func TestBalanceRampsFromAppliedAllocationAfterAPIFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	utils.WriteLeg(utils.LegBoth)
	var reject atomic.Bool
	reject.Store(true)
	var mu sync.Mutex
	now := map[string]string{ProxySelectorTag: RealityLegTag, Hy2ProbeSelectorTag: "proxy-bw-100"}
	for i := 0; i < BalanceLanes; i++ {
		now[BalanceLaneTag(i)] = ProxySelectorTag
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": now[tag]})
			return
		}
		if reject.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		now[tag] = req.Name
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	s := &Supervisor{hasReality: true, verifyLegEgress: true, schedule: BandwidthSchedule{OffDown: 100}, state: &utils.State{Balance: &utils.BalanceStatus{Supported: true}}, clash: NewClashClient(api.Listener.Addr().String()), legProbeFn: func(int, int) (time.Duration, error) { return cleanLeg.delay, nil }}
	var p balancePolicy
	for i := 0; i < 6; i++ {
		s.balanceOnce(&p)
	}
	if s.state.Balance.Enabled {
		t.Fatal("failed API writes claimed dual traffic")
	}
	reject.Store(false)
	s.balanceOnce(&p)
	// Both legs healthy → fixed split
	if s.state.Balance.Hy2Lanes != DefaultHy2Lanes {
		t.Fatalf("recovery from API failure: expected %d lanes, got %d", DefaultHy2Lanes, s.state.Balance.Hy2Lanes)
	}
}
