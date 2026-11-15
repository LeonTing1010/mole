package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

const allocationBucket = 10 * time.Millisecond

func allocationURLs(hy2Lanes int) []string {
	urls := make([]string, BalanceLanes)
	for lane := range urls {
		port := BrutalKeeperPort
		if balanceLaneTarget(lane, hy2Lanes) == Hy2ProbeSelectorTag {
			port = LegGatePort
		}
		urls[lane] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return urls
}

type allocationByteBucket struct {
	start time.Duration
	bytes int64
}
type allocationStream struct {
	bytes   int64
	header  time.Duration
	buckets []allocationByteBucket
	err     error
}

// measureAllocationURLs starts its throughput window only after every response
// header and the warm-up have arrived. Ten-millisecond buckets bound memory
// without confusing another stream's connection startup with payload speed.
func measureAllocationURLs(ctx context.Context, client *http.Client, urls []string, size int, warmup, minWindow time.Duration) (allocationObservation, error) {
	var out allocationObservation
	if len(urls) == 0 || size <= 0 || warmup < 0 || minWindow <= 0 {
		return out, fmt.Errorf("invalid measurement budget")
	}
	samples := make([]allocationStream, len(urls))
	start := time.Now()
	var wg sync.WaitGroup
	for i, base := range urls {
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			r := &samples[i]
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/bulk?bytes=%d", base, size), nil)
			if err != nil {
				r.err = err
				return
			}
			resp, err := client.Do(req)
			r.header = time.Since(start)
			if err != nil {
				r.err = err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				r.err = fmt.Errorf("HTTP %d", resp.StatusCode)
				return
			}
			reader := io.LimitReader(resp.Body, int64(size)+1)
			buf := make([]byte, 32<<10)
			for {
				n, e := reader.Read(buf)
				r.bytes += int64(n)
				if n > 0 {
					bucket := (time.Since(start) / allocationBucket) * allocationBucket
					if len(r.buckets) > 0 && r.buckets[len(r.buckets)-1].start == bucket {
						r.buckets[len(r.buckets)-1].bytes += int64(n)
					} else {
						r.buckets = append(r.buckets, allocationByteBucket{bucket, int64(n)})
					}
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					r.err = e
					return
				}
			}
			if r.bytes != int64(size) {
				r.err = fmt.Errorf("incomplete payload %d/%d", r.bytes, size)
			}
		}(i, base)
	}
	wg.Wait()
	out.Duration = time.Since(start)
	headers := make([]time.Duration, 0, len(samples))
	cut := warmup
	for _, r := range samples {
		if r.err != nil {
			return out, r.err
		}
		out.TotalBytes += r.bytes
		headers = append(headers, r.header)
		if r.header > cut {
			cut = r.header
		}
	}
	// Drop the partial boundary bucket, avoiding credit for bytes received while
	// one stream was still starting.
	cut = ((cut + allocationBucket - 1) / allocationBucket) * allocationBucket
	for _, r := range samples {
		var steady int64
		for _, b := range r.buckets {
			if b.start >= cut {
				steady += b.bytes
			}
		}
		if steady < int64(size)/4 {
			return out, fmt.Errorf("insufficient post-warmup payload")
		}
		out.SteadyBytes += steady
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	window := out.Duration - cut
	out.Midpoint = start.Add(cut + window/2)
	if window < minWindow {
		return out, fmt.Errorf("observation window too short: %s", window)
	}
	slices.Sort(headers)
	out.HeaderMedian = headers[len(headers)/2]
	out.Mbps = float64(out.SteadyBytes) * 8 / window.Seconds() / 1e6
	return out, nil
}
