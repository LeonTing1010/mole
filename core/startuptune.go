package core

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

const (
	StartupTuneSelector = "startup-tune-hy2"
	StartupTuneInbound  = "startup-tune-in"
	StartupTunePort     = 8104
	startupTuneStreams  = 4

	startupTuneDelay  = 12 * time.Second
	startupTuneBudget = 36 * time.Second
	startupTuneGain   = 1.20
)

// StartupTuneBaseline reuses a measured rung only if this ladder still has it.
func (s BandwidthSchedule) StartupTuneBaseline(seed, cached int, now time.Time) int {
	if s.HasRung(cached) {
		return cached
	}
	if s.HasRung(seed) {
		return seed
	}
	_, _, down := s.Profile(now)
	return down
}

func (s *Supervisor) startupTuneActive(leg string) bool {
	return leg == utils.LegHy2 || !s.hasReality && leg == utils.LegAuto
}

// Probe a private selector so foreground connections stay on the startup rung.
func (s *Supervisor) tuneStartupCeiling(ctx context.Context, setting utils.CeilingSetting, baseline int) int {
	ctx, cancel := context.WithTimeout(ctx, startupTuneBudget)
	defer cancel()
	generation := s.probeGeneration.Load()
	leg := utils.ReadLeg()
	stable := func() bool {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		return s.startupTuneActive(leg) && utils.ReadLeg() == leg &&
			utils.ReadCeilingSetting() == setting && s.probeGeneration.Load() == generation &&
			s.healthUsable()
	}
	baselineLive := func() bool {
		member, err := s.clash.Selection(ProxySelectorTag)
		return err == nil && member == BandwidthRungTag(baseline)
	}
	if !stable() || !baselineLive() {
		log.Printf("startup ceiling: skipping benchmark; the baseline is not ready")
		return baseline
	}
	candidates := startupTuneCandidates(s.schedule.Rungs(), baseline)
	if len(candidates) < 2 {
		return baseline
	}
	// Use the same payload for every rung, small enough to finish on
	// low-bandwidth ladders and capped at 4 MiB per sample.
	bytesPerStream := min(1<<20, min(slices.Min(candidates), 40)*125_000/startupTuneStreams)
	winner := measureStartupCeiling(ctx, candidates, func(ctx context.Context, down int) (float64, error) {
		if !stable() {
			cancel()
			return 0, context.Canceled
		}
		return s.measureStartupRung(ctx, down, bytesPerStream)
	})
	if winner != baseline && (!baselineLive() || !stable() || ctx.Err() != nil) {
		return baseline
	}
	return winner
}

// Compare at most two nearby rungs against the known working setting. At a
// ladder edge both alternatives come from the same side.
func startupTuneCandidates(rungs []int, baseline int) []int {
	index, ok := slices.BinarySearch(rungs, baseline)
	if !ok {
		return nil
	}
	result := []int{baseline}
	for distance := 1; distance < len(rungs) && len(result) < 3; distance++ {
		if index-distance >= 0 {
			result = append(result, rungs[index-distance])
		}
		if index+distance < len(rungs) && len(result) < 3 {
			result = append(result, rungs[index+distance])
		}
	}
	return result
}

// Reverse the order in the second pass to reduce sensitivity to changing
// conditions. Require a material win in both passes and two valid baselines.
func measureStartupCeiling(ctx context.Context, candidates []int, measure func(context.Context, int) (float64, error)) int {
	baseline := candidates[0]
	samples := make([][2]float64, len(candidates))
	for pass := range 2 {
		for i := range candidates {
			if ctx.Err() != nil {
				return baseline
			}
			index := i
			if pass == 1 {
				index = len(candidates) - 1 - i
			}
			if speed, err := measure(ctx, candidates[index]); err == nil && speed > 0 {
				samples[index][pass] = speed
			}
		}
	}
	if ctx.Err() != nil || samples[0][0] == 0 || samples[0][1] == 0 {
		return baseline
	}
	winner, best := baseline, startupTuneGain
	for i, sample := range samples[1:] {
		gain := min(sample[0]/samples[0][0], sample[1]/samples[0][1])
		if gain > best {
			winner, best = candidates[i+1], gain
		}
	}
	return winner
}

func (s *Supervisor) measureStartupRung(ctx context.Context, down, bytesPerStream int) (float64, error) {
	if err := s.clash.selectProxy(ctx, StartupTuneSelector, BandwidthRungTag(down)); err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	// Only complete bodies from the node's existing keeper count.
	speed, err := bulkConcurrent(ctx, client, fmt.Sprintf("http://127.0.0.1:%d", StartupTunePort), startupTuneStreams, bytesPerStream)
	if err != nil {
		log.Printf("startup ceiling: ↓%d Mbps probe failed: %v", down, err)
	} else {
		log.Printf("startup ceiling: ↓%d Mbps delivered %.1f Mbps to client", down, speed)
	}
	return speed, err
}
