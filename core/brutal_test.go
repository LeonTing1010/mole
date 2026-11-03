package core

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// Quality/what: an incomplete or unrelated body cannot qualify a transport.
func TestKeeperRejectsIncompleteAndUnrelatedReplies(t *testing.T) {
	// Why: ignoring read errors can qualify a path that stops carrying mid-body.
	for _, broken := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if broken {
				w.Header().Set("Content-Length", "1000")
				w.Write([]byte("rate=65\napplied=1\npeers=1\n"))
			} else {
				w.Write([]byte("<html>unrelated response</html>"))
			}
		}))
		_, err := brutalGET(srv.URL, 1000)
		srv.Close()
		if err == nil {
			t.Fatalf("broken=%v: unreadable keeper reply qualified the leg", broken)
		}
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
func TestBrutalRateURLPointsAtTheForwarder(t *testing.T) {
	if got := BrutalRateURL(65); got != "http://127.0.0.1:8098/rate?mbps=65" {
		t.Errorf("rate url = %q", got)
	}
}

// Quality / what: optional pacing never bypasses authenticated recovery.
// Why: an unavailable candidate must stay rejected with or without Brutal.
func TestUnpacedRealityStillRequiresAuthenticatedRecovery(t *testing.T) {
	newSupervisor := func() *Supervisor {
		s := &Supervisor{
			hasReality:  true,
			probeTimeMs: 200,
			clash:       NewClashClient("127.0.0.1:1"), // nothing listening
			legProbeFn:  func(int, int) (time.Duration, error) { return 0, errors.New("keeper unavailable") },
			state:       &utils.State{}, // link good
		}
		// On the fallback since long enough that a return is due.
		s.fallback.enter(time.Now().Add(-2 * time.Hour))
		return s
	}

	// Control: with pacing confirmed, the return is attempted, the unavailable
	// keeper fails it, and the dwell is restarted — so it is no longer due.
	// Inject the network failure to avoid contacting a user's live tunnel.
	control := newSupervisor()
	control.tryReturnToPrimary()
	if control.fallback.dueBack(time.Now()) {
		t.Fatal("control did not reach the return attempt; the rest of this test proves nothing")
	}

	// Pacing is unconfirmed, but the same failed delivery test still defers recovery.
	unpaced := newSupervisor()
	unpaced.brutalUnpaced.Store(true)
	unpaced.tryReturnToPrimary()
	if unpaced.fallback.dueBack(time.Now()) || !unpaced.onFallback() {
		t.Error("unconfirmed pacing suppressed the failed-delivery recovery test")
	}
}

// Quality / what-x-what: reachable transport is independent of pacing capability.
func TestLegProbeAcceptsReachabilityWithoutPacing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reachable" {
			t.Errorf("wrong transport check %s", r.URL.Path)
		}
		w.Write([]byte("rate=100\napplied=0\npeers=0\nreachable=1\n"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	n, _ := strconv.Atoi(port)
	if _, err := LegProbe(n, 1000); err != nil {
		t.Fatal(err)
	}
}
