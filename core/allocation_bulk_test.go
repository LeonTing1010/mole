package core

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
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
			if r.URL.Query().Get("bytes") != "1024" {
				return nil, fmt.Errorf("unequal payload")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 1024)))}, nil
		})}
		rate, err := allocationBulk(client, share, 1024)
		if err != nil || rate <= 0 {
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
	if rate, err := allocationBulk(client, 2, 1024); err == nil || rate != 0 {
		t.Fatalf("partial mixed result qualified: %v %v", rate, err)
	}
	for _, share := range []int{-1, 9} {
		if _, err := allocationBulk(client, share, 1024); err == nil {
			t.Fatal("invalid share accepted")
		}
	}
}
