package core

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type governorSample struct {
	mbps   float64
	loaded time.Duration // zero when the short request did not complete during bulk
}

// Keep the 32 MiB reference burst on fast paths. On slow rungs budget eight
// seconds at the declaration so the 20-second request timeout permits recovery.
func governorStreamBytes(rate int) int {
	return min(4<<20, max(1, rate)*125000)
}

// Time a one-byte keeper response on the same leg after bulk starts receiving.
// A late or failed response cannot qualify an upward trial.
func measureGovernor(ctx context.Context, client *http.Client, url string, bytesPerStream int) (governorSample, error) {
	probeCtx, stopProbe := context.WithCancel(ctx)
	defer stopProbe()
	started := make(chan struct{})
	var once sync.Once
	bulkCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { once.Do(func() { close(started) }) }})
	latency := make(chan time.Duration, 1)
	go func() {
		select {
		case <-probeCtx.Done():
			latency <- 0
			return
		case <-started:
		}
		latency <- loadedKeeperLatency(probeCtx, client, url)
	}()
	speed, err := bulkConcurrent(bulkCtx, client, url, 8, bytesPerStream)
	stopProbe()
	return governorSample{speed, <-latency}, err
}

func loadedKeeperLatency(ctx context.Context, client *http.Client, url string) time.Duration {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/bulk?bytes=1", nil)
	if err != nil {
		return 0
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 2))
	if err != nil || resp.StatusCode != http.StatusOK || n != 1 || ctx.Err() != nil {
		return 0
	}
	return time.Since(start)
}

const (
	recoveryHoldSamples = 3 // 30 minutes at the normal cadence
	recoveryMinGain     = 1.10
)

// sampleCycle performs the reversible trial in the same tick so a failed
// candidate cannot remain the target for another ten minutes. setRate returns
// true only after the node acknowledges that exact declaration.
func (g *RateGovernor) sampleCycle(measure func() (governorSample, error), setRate func(int) bool) {
	before := g.Rate()
	base, err := measure()
	if err != nil {
		g.neutral = 0
		log.Printf("rate governor: hold ↓%d Mbps; baseline probe failed: %v", before, err)
		return
	}
	anchored := g.anchor
	next, changed, why := g.Observe(base.mbps)
	log.Printf("rate governor: sample ↓%d Mbps delivered %.1f Mbps, loaded %v, next ↓%d (%s)", before, base.mbps, base.loaded, next, why)
	ratio := base.mbps / float64(before)
	if anchored || changed || !g.farBelowCeiling() || ratio < govLowRatio || ratio >= govHighRatio || base.loaded <= 0 {
		g.neutral = 0
		return
	}
	g.neutral++
	if g.neutral < recoveryHoldSamples {
		return
	}
	g.neutral = 0
	candidate := g.clamp(int(float64(before)*govFarClimbScale) + 1)
	if setRate(candidate) {
		trial, err := measure()
		// Require meaningful throughput gain; allow modest RTT jitter at low
		// latency and at most 25% extra delay on longer paths.
		limit := base.loaded + max(25*time.Millisecond, base.loaded/4)
		keep := err == nil && trial.mbps >= base.mbps*recoveryMinGain && trial.loaded > 0 && trial.loaded <= limit
		log.Printf("rate governor: trial ↓%d Mbps delivered %.1f Mbps, loaded %v (limit %v), baseline %.1f Mbps, keep=%t, error=%v", candidate, trial.mbps, trial.loaded, limit, base.mbps, keep, err)
		if keep {
			g.move(candidate, "recovery trial")
			return
		}
	} else {
		log.Printf("rate governor: trial ↓%d Mbps unconfirmed", candidate)
	}
	// Even a lost acknowledgement can mean the candidate was applied remotely.
	log.Printf("rate governor: restore ↓%d Mbps, confirmed=%t", before, setRate(before))
}
