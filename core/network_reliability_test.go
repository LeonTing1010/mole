package core

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func networkFixSelector(t *testing.T, initial string) (*ClashClient, *atomic.Value) {
	t.Helper()
	selected := &atomic.Value{}
	selected.Store(initial)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var body struct{ Name string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			selected.Store(body.Name)
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/delay") {
			io.WriteString(w, `{"delay":50}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"now": selected.Load().(string)})
	}))
	t.Cleanup(api.Close)
	return NewClashClient(api.Listener.Addr().String()), selected
}

// Quality / what: abandoning a broken fallback must commit the real route
// before clearing its evidence. This exercises keepaliveOnce, not the helper.
func TestNetworkFixAbandonFallbackCommitsActualRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clash, selected := networkFixSelector(t, "proxy-bw-400")
	s := &Supervisor{hasReality: true, clash: clash, probeTimeMs: 100, schedule: BandwidthSchedule{OffDown: 400, OffUp: 400},
		state:  &utils.State{KeepaliveFails: fallbackEnterFails - 1, LastProbeVerdict: utils.ProbeAlive},
		stopCh: make(chan struct{}), legProbeFn: func(int, int) (time.Duration, error) { return 50 * time.Millisecond, nil }}
	s.fallback.enter(time.Now())
	s.tunnelClientOnce.Do(func() {
		s.tunnelHTTP = &http.Client{Transport: probeRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("HY2 transport unavailable")
		})}
	})
	s.keepaliveOnce()
	t.Logf("fallback=%v actual_selector=%s failure_count=%d", s.onFallback(), selected.Load(), s.state.KeepaliveFails)
	if s.onFallback() || selected.Load() != RealityLegTag || s.state.KeepaliveFails != 0 {
		t.Fatal("healthy candidate must receive traffic before fallback evidence is cleared")
	}
}

// Quality / what: a single origin's HTTP failure is not transport failure.
func TestNetworkFixSingleOriginFailureDoesNotMoveHealthyTransport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clash, selected := networkFixSelector(t, RealityLegTag)
	s := &Supervisor{hasReality: true, clash: clash, probeTimeMs: 100, schedule: BandwidthSchedule{OffDown: 400, OffUp: 400},
		state:  &utils.State{KeepaliveFails: fallbackEnterFails - 1, LastProbeVerdict: utils.ProbeAlive},
		stopCh: make(chan struct{}), legProbeFn: func(int, int) (time.Duration, error) { return 50 * time.Millisecond, nil }}
	client := &http.Client{Transport: probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		status := 200
		if r.URL.Host == "cloudflare.com" {
			status = 503
		} else if r.URL.Host == "www.gstatic.com" {
			status = 204
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(func() string {
			if status == 204 {
				return ""
			}
			return "payload"
		}())), Header: make(http.Header)}, nil
	})}
	s.tunnelClientOnce.Do(func() { s.tunnelHTTP = client })
	if _, err := s.fetchThroughTunnel(client, "https://independent.example/health"); err != nil {
		t.Fatal(err)
	}
	s.keepaliveOnce()
	t.Logf("independent origin delivered payload; actual_selector=%s fallback=%v", selected.Load(), s.onFallback())
	if selected.Load() != RealityLegTag || s.onFallback() || s.state.KeepaliveFails != 0 {
		t.Fatal("one origin's 503 moved a transport that demonstrably still delivered another origin's payload")
	}
}

// Quality / what: a listener without authenticated delivery cannot rescue HY2.
// Why: clearing evidence on a SYN-ACK used to hide a still-broken route.
func TestNetworkFixRescueKeepsEvidenceWhenCandidateDoesNotDeliver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clash, selected := networkFixSelector(t, "proxy-bw-400")
	s := &Supervisor{hasReality: true, clash: clash, state: &utils.State{KeepaliveFails: 9},
		stopCh: make(chan struct{}), legProbeFn: func(int, int) (time.Duration, error) {
			return 0, errors.New("authenticated payload not delivered")
		}}
	s.fallback.enter(time.Now())
	s.rescueStuckFallback()
	if !s.onFallback() || selected.Load() != "proxy-bw-400" || s.state.KeepaliveFails != 9 {
		t.Fatal("failed recovery changed route or discarded failure evidence")
	}
}

// Quality / what: independent origin checks still count a real delivery outage.
func TestNetworkFixBothOriginsFailStillRecordsFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &Supervisor{probeTimeMs: 100, state: &utils.State{}}
	s.tunnelClientOnce.Do(func() {
		s.tunnelHTTP = &http.Client{Transport: probeRoundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("both origins unreachable")
		})}
	})
	s.keepaliveOnce()
	if s.state.KeepaliveFails != 1 || s.state.LastKeepaliveError == "" {
		t.Fatal("two failed origins must retain delivery failure evidence")
	}
}

// Quality / what: optional pacing cannot suppress authenticated recovery.
// Why: a missing kernel optimization must not strand a usable transport.
func TestNetworkFixOptionalPacingDoesNotBlockRecoveryMeasurement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clash, selected := networkFixSelector(t, "proxy-bw-400")
	var calls atomic.Int32
	s := &Supervisor{hasReality: true, clash: clash, probeTimeMs: 100, schedule: BandwidthSchedule{OffDown: 400, OffUp: 400},
		state: &utils.State{}, stopCh: make(chan struct{}),
		legProbeFn: func(int, int) (time.Duration, error) { calls.Add(1); return 50 * time.Millisecond, nil }}
	s.fallback.enter(time.Now().Add(-2 * fallbackMinDwell))
	s.brutalUnpaced.Store(true)
	s.tryReturnToPrimary()
	if calls.Load() != legGateProbes || s.onFallback() || selected.Load() != RealityLegTag {
		t.Fatal("optional pacing blocked authenticated candidate recovery")
	}
	t.Log("expired dwell tested and recovered REALITY without requiring optional TCP pacing")
}
