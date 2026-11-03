package core

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

// The two keepalive arms differ in exactly one property, and if that property
// silently stops holding, the fresh arm keeps reporting numbers that mean
// nothing — which is the same way the original single-arm design went blind.
// So assert the property itself, not the latency.
//
// Why this is worth a test: a Transport reuses connections by default, HTTP/2
// multiplexes over one whether you asked for it or not, and TLS session
// resumption can turn a "fresh" handshake into a resumed one. Any of the three
// would quietly turn this arm back into the warm arm.
func TestKeepaliveArmsDifferInConnectionReuse(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]int{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fl=test\n") // a body, like the real keepalive URL
	}))
	srv.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st != http.StateNew {
			return
		}
		mu.Lock()
		conns[c.RemoteAddr().String()]++
		mu.Unlock()
	}
	srv.Start()
	defer srv.Close()

	const beats = 5
	count := func(c *http.Client) int {
		mu.Lock()
		conns = map[string]int{}
		mu.Unlock()
		for i := 0; i < beats; i++ {
			if _, err := (&Supervisor{}).fetchThroughTunnel(c, srv.URL); err != nil {
				t.Fatalf("beat %d: %v", i, err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return len(conns)
	}

	s := &Supervisor{probeTimeMs: 5000}

	if got := count(s.tunnelClient()); got != 1 {
		t.Errorf("warm arm opened %d connections over %d beats, want 1 — it is supposed to hold one session", got, beats)
	}
	if got := count(s.dialClient()); got != beats {
		t.Errorf("fresh arm opened %d connections over %d beats, want %d — it is measuring reuse, not dialing", got, beats, beats)
	}
}

// An empty body is a failure for both arms: a tunnel that connects and carries
// nothing is the exact case the keepalive was rewritten to catch.
func TestFetchThroughTunnelRejectsEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200, no body
	}))
	defer srv.Close()

	s := &Supervisor{probeTimeMs: 5000}
	for _, arm := range []struct {
		name string
		c    *http.Client
	}{{"warm", s.tunnelClient()}, {"fresh", s.dialClient()}} {
		if _, err := s.fetchThroughTunnel(arm.c, srv.URL); err == nil {
			t.Errorf("%s arm: empty body accepted as healthy", arm.name)
		}
	}
}

// A failed dial must record 0ms, not the time the timeout spent — otherwise the
// window fills with numbers describing the timeout setting rather than the path.
func TestRecordDialOnFailureStoresNoLatency(t *testing.T) {
	st := &utils.State{}
	st.RecordDial(4999, fmt.Errorf("context deadline exceeded"))
	if st.LastDialMs != 0 {
		t.Errorf("LastDialMs = %d, want 0", st.LastDialMs)
	}
	if st.DialSamples[0].Ms != 0 || st.DialSamples[0].OK {
		t.Errorf("sample = %+v, want {Ms:0 OK:false}", st.DialSamples[0])
	}
	if st.LastDialError == "" {
		t.Error("LastDialError empty after a failed dial")
	}
	st.RecordDial(210, nil)
	if st.LastDialError != "" {
		t.Errorf("LastDialError = %q, want it cleared by the next good dial", st.LastDialError)
	}
}
