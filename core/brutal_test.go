package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// The bug this file exists for: on 2026-09-09 the REALITY leg carried
// 0.15–0.48 Mbps while the hy2 ladder carried 38–65 in the same minutes, and
// the daemon logged "brutal: node paces TCP toward this client at ↓100 Mbps"
// throughout. The rate push went out as a Clash delay probe, sing-box 1.13's
// delay endpoint ignores the url it is given, and so the success branch fired
// on a request that was never sent. Everything below is about one property:
// the confirmation has to be able to come back false.

func TestBrutalReplyIsParsedByKey(t *testing.T) {
	res := parseBrutalReply("rate=65\napplied=1\npeers=2\ndetail=whatever it said\nfuture-key=ignored\n")
	if res.Rate != 65 || res.Applied != 1 || res.Peers != 2 {
		t.Fatalf("got %+v", res)
	}
	if res.Detail != "whatever it said" {
		t.Errorf("detail = %q", res.Detail)
	}
}

// A keeper that heard the rate and could not install it answers 503 with
// applied=0. That is a different failure from silence and has to survive the
// trip as one, because it is the one that names a cause.
func TestKeeperRefusalIsAnErrorThatCarriesTheReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("rate=100\napplied=0\npeers=1\ndetail=kernel module 'brutal' is not loaded\n"))
	}))
	defer srv.Close()

	res, err := brutalGET(srv.URL, 2000)
	if err == nil {
		t.Fatal("a keeper that did not apply the rate must not read as success")
	}
	if res.Applied != 0 || res.Peers != 1 {
		t.Errorf("got %+v", res)
	}
	if res.Detail != "kernel module 'brutal' is not loaded" {
		t.Errorf("the reason did not survive: %q", res.Detail)
	}
}

func TestKeeperSuccessReportsWhatItInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("mbps"); got != "" && got != "65" {
			t.Errorf("keeper was asked for mbps=%q", got)
		}
		_, _ = w.Write([]byte("rate=65\napplied=1\npeers=1\n"))
	}))
	defer srv.Close()

	res, err := brutalGET(srv.URL+"/rate?mbps=65", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || res.Rate != 65 {
		t.Fatalf("got %+v", res)
	}
}

// An unreachable keeper is the third outcome, and the one the old code could
// not produce at all.
func TestUnreachableKeeperIsAnError(t *testing.T) {
	if _, err := brutalGET("http://127.0.0.1:1/rate?mbps=100", 500); err == nil {
		t.Fatal("a keeper that is not there must not read as success")
	}
}

// The rate URL names the client's own loopback because the forwarder is what
// carries it: a destination of 127.0.0.1 is written on the node's side of the
// leg, which is the only place it means anything.
func TestBrutalURLsPointAtTheForwarder(t *testing.T) {
	if got := BrutalRateURL(65); got != "http://127.0.0.1:8098/rate?mbps=65" {
		t.Errorf("rate url = %q", got)
	}
	if got := BrutalBulkURL(8000000); got != "http://127.0.0.1:8098/bulk?bytes=8000000" {
		t.Errorf("bulk url = %q", got)
	}
}

// A leg that dials in 300ms and then carries 0.4 Mbps passes every health
// signal mole has, because dialling is exactly what keeps working when Brutal
// is missing. So the unpaced finding has to block the handback itself.
func TestUnpacedRealityIsNotHandedTrafficBack(t *testing.T) {
	newSupervisor := func() *Supervisor {
		s := &Supervisor{
			hasReality:  true,
			probeTimeMs: 200,
			clash:       NewClashClient("127.0.0.1:1"), // nothing listening
			state:       &utils.State{},                // link good
		}
		// On the fallback since long enough that a return is due.
		s.fallback.enter(time.Now().Add(-2 * time.Hour))
		return s
	}

	// Control: with pacing confirmed, the return is attempted, the dead Clash
	// API fails it, and the dwell is restarted — so it is no longer due.
	control := newSupervisor()
	control.tryReturnToPrimary()
	if control.fallback.dueBack(time.Now()) {
		t.Fatal("control did not reach the return attempt; the rest of this test proves nothing")
	}

	// The case: unpaced, so the attempt is never made and the dwell is
	// untouched.
	unpaced := newSupervisor()
	unpaced.brutalUnpaced.Store(true)
	unpaced.tryReturnToPrimary()
	if !unpaced.fallback.dueBack(time.Now()) {
		t.Error("traffic was handed back toward a leg the node will not pace")
	}
}
