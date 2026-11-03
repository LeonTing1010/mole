package core

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

type probeRoundTrip func(*http.Request) (*http.Response, error)

func (f probeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Quality/what-x-what: in-flight results from the old leg cannot convict the new one.
func TestTransportChangeDiscardsInFlightProbeEvidence(t *testing.T) {
	// Why: a scheduler handoff can happen while the old HTTP request is timing out.
	t.Setenv("HOME", t.TempDir())
	for _, warm := range []bool{false, true} {
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		client := &http.Client{Transport: probeRoundTrip(func(*http.Request) (*http.Response, error) {
			once.Do(func() { close(entered) })
			<-release
			return nil, errors.New("old transport timed out")
		})}
		s := &Supervisor{state: &utils.State{KeepaliveFails: 3}}
		if warm {
			s.tunnelClientOnce.Do(func() { s.tunnelHTTP = client })
		} else {
			s.dialClientOnce.Do(func() { s.dialHTTP = client })
		}
		go func() {
			defer close(done)
			if warm {
				s.keepaliveOnce()
			} else {
				s.dialOnce()
			}
		}()
		<-entered
		s.resetKeepaliveCount()
		close(release)
		<-done
		if s.state.KeepaliveFails != 0 || len(s.state.DialSamples) != 0 {
			t.Fatalf("warm=%v: stale failure reached the new leg: fails=%d dials=%d", warm, s.state.KeepaliveFails, len(s.state.DialSamples))
		}
	}
}
