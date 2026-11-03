package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Quality/what: a broken standby is not protected by its minimum dwell.
func TestFreshDialGateCanReconsiderAFailingFallback(t *testing.T) {
	// Why: warm connections can survive while every new flow on UDP times out.
	if !legTestDue(true, true, utils.LegAuto, t0, time.Time{}) {
		t.Fatal("failing fallback cannot compare the recovered primary before dwell")
	}
	for _, pin := range []string{utils.LegHy2, utils.LegReality} {
		if legTestDue(true, true, pin, t0, time.Time{}) {
			t.Fatalf("manual pin %q was overridden", pin)
		}
	}
}

// Quality/what-x-what: a healthy TCP leg can rescue broken UDP fresh flows.
func TestFreshDialFailuresEscapeFallbackBeforeDwell(t *testing.T) {
	// Why: the October 7 outage had healthy warm flows and silent UDP probes.
	t.Setenv("HOME", t.TempDir())
	var selected atomic.Value
	selected.Store("proxy-bw-400")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			selected.Store(RealityLegTag)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"now":"proxy-bw-400"}`))
	}))
	defer api.Close()
	s := &Supervisor{
		hasReality: true, state: &utils.State{}, stopCh: make(chan struct{}),
		clash: NewClashClient(api.Listener.Addr().String()),
		legProbeFn: func(port, _ int) (time.Duration, error) {
			if port == LegGatePort {
				return 5 * time.Second, errors.New("UDP fresh flow timeout")
			}
			return 200 * time.Millisecond, nil
		},
	}
	s.fallback.enter(time.Now())
	for i := 0; i < utils.DialBadEnter; i++ {
		s.state.RecordDial(0, errors.New("timeout"))
	}
	s.considerLegFailover()
	if s.onFallback() || selected.Load() != RealityLegTag {
		t.Fatalf("broken fallback retained: on=%v selected=%s", s.onFallback(), selected.Load())
	}
	if s.state.SlowNewFlows || len(s.state.DialSamples) != 0 {
		t.Fatal("old transport evidence survived the handoff")
	}
}

// The rule that stopped the 2026-10-14 ping-pong. The comparative gate opens on
// new connections that FAILED, never on ones that merely answered slowly: all
// six of that evening's switches were opened by four 1.1-2.4s SUCCESSES, and
// each one moved traffic onto a leg whose next real connections timed out.
func TestFreshFlowsFailingIgnoresSlowDials(t *testing.T) {
	var slow utils.State
	for i := 0; i < utils.DialWindow; i++ {
		slow.RecordDial(utils.DialSlowMs+500, nil)
	}
	if freshFlowsFailing(&slow) {
		t.Fatalf("a window of slow-but-successful dials opened the leg gate (bad=%d, fails=%d)",
			slow.DialBad(), slow.DialFails())
	}
	var dead utils.State
	for i := 0; i < utils.DialBadEnter; i++ {
		dead.RecordDial(0, errors.New("timeout"))
	}
	if !freshFlowsFailing(&dead) {
		t.Fatalf("four failed dials did not open the leg gate (fails=%d)", dead.DialFails())
	}
}

// Quality/what-x-what: a failed selector write cannot announce recovery.
func TestFailedPrimarySelectionKeepsFallbackAndEvidence(t *testing.T) {
	// Why: clearing fallback first leaves status and routing on different legs.
	t.Setenv("HOME", t.TempDir())
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer api.Close()
	s := &Supervisor{
		hasReality: true, state: &utils.State{KeepaliveFails: 2}, stopCh: make(chan struct{}),
		clash:      NewClashClient(api.Listener.Addr().String()),
		legProbeFn: func(int, int) (time.Duration, error) { return 200 * time.Millisecond, nil },
	}
	s.fallback.enter(time.Now().Add(-2 * fallbackMinDwell))
	s.tryReturnToPrimary()
	if !s.onFallback() || s.state.KeepaliveFails != 2 {
		t.Fatalf("failed write announced recovery: fallback=%v failures=%d", s.onFallback(), s.state.KeepaliveFails)
	}
}
