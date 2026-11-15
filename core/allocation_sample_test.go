package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Quality/what: complete downloads exclude the warm-up interval from the rate.
func TestAllocationMeasurementExcludesWarmupAndRejectsShortSamples(t *testing.T) {
	// Why: connection startup and the slowest fresh stream dominated 1 MiB scores.
	const bytes = 8192
	client := &http.Client{Transport: bulkSpeedTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(&pacedBody{first: strings.NewReader(strings.Repeat("a", bytes/2)), second: strings.NewReader(strings.Repeat("b", bytes/2))})}, nil
	})}
	before := time.Now()
	obs, err := measureAllocationURLs(context.Background(), client, []string{"http://sample.invalid"}, bytes, 30*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Midpoint.Sub(before) < obs.Duration/2+10*time.Millisecond {
		t.Fatal("sample midpoint includes excluded warm-up")
	}
	if obs.SteadyBytes != bytes/2 || obs.TotalBytes != bytes || obs.Mbps <= 0 {
		t.Fatalf("warm-up incorrectly counted: %+v", obs)
	}
	_, err = measureAllocationURLs(context.Background(), client, []string{"http://sample.invalid"}, bytes, 30*time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("short observation qualified")
	}
}

type pacedBody struct {
	first, second *strings.Reader
	waited        bool
}

func (b *pacedBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	if !b.waited {
		time.Sleep(70 * time.Millisecond)
		b.waited = true
	}
	return b.second.Read(p)
}

// Quality/what: completed streams cannot disappear from a steady-state comparison.
func TestAllocationMeasurementRejectsStreamsFinishedBeforeAllHeaders(t *testing.T) {
	// Why: a late REALITY request must not make earlier HY2 traffic disappear.
	client := &http.Client{Transport: bulkSpeedTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "slow.invalid" {
			time.Sleep(40 * time.Millisecond)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(&pacedBody{first: strings.NewReader(strings.Repeat("a", 4096)), second: strings.NewReader(strings.Repeat("b", 4096))})}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", 8192)))}, nil
	})}
	_, err := measureAllocationURLs(context.Background(), client, []string{"http://fast.invalid", "http://slow.invalid"}, 8192, 30*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("completed stream was silently excluded")
	}
}

// Quality/what-x-what: a real streamed response passes the loaded gateway guard.
func TestGuardedAllocationMeasurementStreamsThroughLocalHTTP(t *testing.T) {
	// Why: exercise the sample, cancellation-aware guard, and body contract together.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8192")
		w.Write([]byte(strings.Repeat("a", 4096)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(80 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(strings.Repeat("b", 4096)))
	}))
	defer server.Close()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: time.Second, Transport: transport}
	urls := make([]string, 8)
	for i := range urls {
		urls[i] = server.URL
	}
	obs, err := guardedAllocationMeasure(context.Background(), 4*time.Millisecond, 10*time.Millisecond,
		func(context.Context) (time.Duration, error) { return 5 * time.Millisecond, nil },
		func() error { return nil },
		func(ctx context.Context) (allocationObservation, error) {
			return measureAllocationURLs(ctx, client, urls, 8192, 30*time.Millisecond, 20*time.Millisecond)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if obs.TotalBytes != 8*8192 || obs.SteadyBytes != 8*4096 || obs.GatewayWorst != 5*time.Millisecond {
		t.Fatalf("unexpected measured body: %+v", obs)
	}
}
