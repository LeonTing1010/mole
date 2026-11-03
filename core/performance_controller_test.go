package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Quality/what-x-what: recovery refresh and slow-response policy reach live selectors.
func TestPerformanceControllerSlowProbeDegradesThenHardFailureWithdraws(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := utils.WriteLeg(utils.LegBoth); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	selected := map[string]string{ProxySelectorTag: RealityLegTag, Hy2ProbeSelectorTag: "proxy-bw-100"}
	for i := 0; i < BalanceLanes; i++ {
		selected[BalanceLaneTag(i)] = ProxySelectorTag
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tag := strings.TrimPrefix(r.URL.Path, "/proxies/")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]string{"now": selected[tag]})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		selected[tag] = req.Name
		w.WriteHeader(204)
	})
	clash := NewClashClient("control.invalid")
	clash.http.Transport = probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	var condition atomic.Int32 // 0=clean, 1=slow, 2=failed.
	s := &Supervisor{
		hasReality: true, verifyLegEgress: true, clash: clash, schedule: BandwidthSchedule{OffDown: 100},
		state: &utils.State{BandwidthDownMbps: 100, Balance: &utils.BalanceStatus{Supported: true}},
		legProbeFn: func(port, _ int) (time.Duration, error) {
			if port == LegGatePort {
				if condition.Load() == 1 {
					return 1200 * time.Millisecond, nil
				}
				if condition.Load() == 2 {
					return brokenLeg.delay, brokenLeg.err
				}
			}
			return cleanLeg.delay, nil
		},
	}
	var p balancePolicy
	for i := 0; i < 12; i++ {
		s.balanceOnce(&p)
	}
	// Both healthy → fixed split
	if s.state.Balance.Hy2Lanes != DefaultHy2Lanes {
		t.Fatalf("both healthy should give %d lanes, got %d", DefaultHy2Lanes, s.state.Balance.Hy2Lanes)
	}
	// Slow probe marks degraded but doesn't withdraw
	condition.Store(1)
	s.balanceOnce(&p)
	if !s.state.Balance.Hy2Healthy || !s.state.Balance.Hy2Degraded || s.state.Balance.Hy2Lanes == 0 {
		t.Fatal("soft degradation should keep lanes")
	}
	// 3 consecutive errors → unhealthy → withdraw
	condition.Store(2)
	s.balanceOnce(&p) // bad=1
	s.balanceOnce(&p) // bad=2
	s.balanceOnce(&p) // bad=3 → unhealthy
	if s.state.Balance.Hy2Lanes != 0 {
		t.Fatal("hard failure should withdraw UDP")
	}
	// Recovery → back to fixed split
	condition.Store(0)
	for i := 0; i < balanceRecoverSamples; i++ {
		s.balanceOnce(&p)
	}
	if s.state.Balance.Hy2Lanes != DefaultHy2Lanes {
		t.Fatalf("recovery should give %d lanes, got %d", DefaultHy2Lanes, s.state.Balance.Hy2Lanes)
	}
}

// Quality/what: external status and payload contracts survive multi-origin retries.
func TestPerformanceEgressValidatesStatusAndCompleteBody(t *testing.T) {
	for _, tc := range []struct {
		status, expected int
		body             string
		required, ok     bool
	}{
		{200, 200, "trace", true, true}, {200, 200, "", true, false}, {204, 204, "", false, true},
		{200, 204, "", false, false}, {204, 204, "unexpected", false, false},
	} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		err := validateEgressResponse(resp, LegEgressTarget{Status: tc.expected, RequireBody: tc.required})
		if (err == nil) != tc.ok {
			t.Fatalf("status=%d body=%q: %v", tc.status, tc.body, err)
		}
	}
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(brokenEgressReader{})}
	if err := validateEgressResponse(resp, LegEgressTarget{Status: 200, RequireBody: true}); err == nil {
		t.Fatal("incomplete external body qualified")
	}
}

type brokenEgressReader struct{}

func (brokenEgressReader) Read([]byte) (int, error) { return 0, fmt.Errorf("truncated response") }
