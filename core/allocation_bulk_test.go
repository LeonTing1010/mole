package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Quality/what: every share must use the same total load and one elapsed window.
func TestAllocationBulkUsesEqualBudgetAcrossEndpointsAndMixtures(t *testing.T) {
	// Why: adding two independent full-concurrency rates invents dual-route gains.
	for share := 0; share <= BalanceLanes; share++ {
		var mu sync.Mutex
		counts := map[string]int{}
		client := &http.Client{Transport: bulkSpeedTransport(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			counts[r.URL.Host]++
			mu.Unlock()
			if r.URL.Query().Get("bytes") != "8192" {
				return nil, fmt.Errorf("unequal payload")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(&pacedBody{first: strings.NewReader(strings.Repeat("a", 4096)), second: strings.NewReader(strings.Repeat("b", 4096))})}, nil
		})}
		obs, err := measureAllocationURLs(context.Background(), client, allocationURLs(share), 8192, 30*time.Millisecond, 20*time.Millisecond)
		if err != nil || obs.Mbps <= 0 || obs.TotalBytes != BalanceLanes*8192 {
			t.Fatalf("share %d: %v", share, err)
		}
		if counts["127.0.0.1:8099"] != share || counts["127.0.0.1:8098"] != BalanceLanes-share {
			t.Fatalf("share %d: %v", share, counts)
		}
	}
}

// Safety/what: failed mixed payloads must never qualify as measured capacity.
func TestAllocationBulkRejectsIncompleteMixture(t *testing.T) {
	client := &http.Client{Transport: bulkSpeedTransport(func(r *http.Request) (*http.Response, error) {
		size := 1024
		if r.URL.Port() == "8099" {
			size--
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", size)))}, nil
	})}
	if obs, err := measureAllocationURLs(context.Background(), client, allocationURLs(2), 1024, 0, time.Millisecond); err == nil || obs.Mbps != 0 {
		t.Fatalf("partial mixed result qualified: %v %v", obs, err)
	}
}
