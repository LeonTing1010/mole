package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Quality/what-x-what. Why: at 73 Mbps a healthy link can stay below the
// 95% climb threshold even with substantial available bandwidth.
func TestGovernorRecoveryTrial(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		trial                    governorSample
		candidateFails, wantKeep bool
	}{
		{"faster", governorSample{80, 110 * time.Millisecond}, false, true},
		{"latency regression", governorSample{80, 200 * time.Millisecond}, false, false},
		{"no meaningful gain", governorSample{68, 100 * time.Millisecond}, false, false},
		{"missing loaded latency", governorSample{80, 0}, false, false},
		{"lost candidate acknowledgement", governorSample{}, true, false},
		{"incomplete trial", governorSample{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewRateGovernor(73, 3, 600)
			declared := 73
			attempts := 0
			set := func(n int) bool {
				declared = n
				attempts++
				return !(tc.candidateFails && n != 73)
			}
			measure := func() (governorSample, error) {
				if declared == 73 {
					return governorSample{65, 100 * time.Millisecond}, nil
				}
				if tc.trial.mbps == 0 {
					return governorSample{}, fmt.Errorf("interrupted")
				}
				return tc.trial, nil
			}
			for i := 0; i < 3; i++ {
				g.sampleCycle(measure, set)
				if i < 2 && attempts != 0 {
					t.Fatal("trial before persistent neutral samples")
				}
			}
			if (declared > 73) != tc.wantKeep || g.Rate() != declared || attempts == 0 {
				t.Fatalf("declared=%d desired=%d pushes=%d", declared, g.Rate(), attempts)
			}
		})
	}
}

// Quality/what. Even the minimum rate needs a payload deliverable before timeout.
func TestGovernorPayloadFitsSlowRungs(t *testing.T) {
	if governorStreamBytes(73) != 4<<20 || 8*governorStreamBytes(3) > 3*125000*8 {
		t.Fatal("probe budget does not fit its rate")
	}
}

// Quality/what-x-what. The short request must complete while bulk is in flight.
func TestGovernorMeasuresLatencyUnderLoad(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
		w.Header().Set("Content-Length", strconv.Itoa(n))
		if n == 1 {
			io.WriteString(w, "x")
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "abcd")
	}))
	defer s.Close()
	tr := bulkSpeedTransport(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if r.URL.Query().Get("bytes") == "1" && err == nil {
			resp.Body = &releaseOnClose{resp.Body, func() { once.Do(func() { close(release) }) }}
		}
		return resp, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := measureGovernor(ctx, &http.Client{Transport: tr}, s.URL, 4)
	if err != nil || got.mbps <= 0 || got.loaded <= 0 {
		t.Fatalf("sample=%+v error=%v", got, err)
	}
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
}

func (r *releaseOnClose) Close() error {
	r.release()
	return r.ReadCloser.Close()
}
