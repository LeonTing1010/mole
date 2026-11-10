package core

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// BulkConcurrent fires n parallel GETs at baseURL's /bulk endpoint, each
// requesting bytesPerStream bytes, and returns the aggregate throughput in
// Mbps.
//
// The concurrency is the point: mole's traffic rides smux with 8 connections,
// and single-stream and 8-stream optima regularly disagree on real paths. A
// single-stream probe would report a healthy number on a path that collapses
// under the actual workload, which is how gain scanning used to pick wrong.
func BulkConcurrent(baseURL string, n, bytesPerStream int) (float64, error) {
	// Bounded: the caller may be the supervisor's scheduler goroutine, and a
	// keeper that accepts the connection but never drains would otherwise wedge
	// health checks and failover along with the probe.
	client := &http.Client{Timeout: 20 * time.Second}

	var wg sync.WaitGroup
	var errs int32
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			bulkURL := fmt.Sprintf("%s/bulk?bytes=%d", baseURL, bytesPerStream)
			resp, err := client.Get(bulkURL)
			if err != nil {
				atomic.AddInt32(&errs, 1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if errs > 0 {
		return 0, fmt.Errorf("%d/%d streams failed", errs, n)
	}
	if elapsed < 0.1 {
		return 0, fmt.Errorf("elapsed <0.1s (rule not applied?)")
	}
	totalBytes := int64(n) * int64(bytesPerStream)
	return float64(totalBytes*8) / elapsed / 1e6, nil
}
