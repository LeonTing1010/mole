package utils

import (
	"sort"
	"time"
)

// The local-link verdict.
//
// Every other instrument on State measures something on the far side of the
// Wi-Fi: the out-of-tunnel probe, both keepalive arms, the ENOBUFS counter. None
// of them can tell a path that went dark from a radio that did, and that
// distinction decides everything mole does next — a leg change or a ceiling
// descent is only ever an answer to the first. Three and a half days of logs
// (2026-09-03..05) had five degraded episodes, 71 minutes, in which direct DNS
// and direct TCP were failing at the same rate as the tunnel; in the worst of
// them mole changed legs four times in thirty minutes on the strength of a
// single clean probe, and the self-heal walked the ceiling down three rungs on
// ENOBUFS that were the radio refusing to send. This is the instrument that
// was missing: an echo to the first hop, which is the one thing that is on
// this side of the fault line.
//
// Latched (Schmitt trigger), like the fresh-dial verdict, and for the same
// reason: the underlying rate hovers around any single threshold during a bad
// stretch and an unlatched verdict would flap every beat.
const (
	// LinkWindow is how many gateway echoes the verdict looks at. At the 5s
	// cadence this is one minute.
	LinkWindow = 12
	// LinkBadLoss / LinkOKLoss: enter the bad verdict at 3 lost in 12 (25%),
	// leave it at 1 or fewer (8%). Measured 2026-08-27 on a bad evening: the
	// gateway hop lost 8-11% idle with 993ms peaks; a healthy hop loses none.
	// Since 2026-10-14 the loss arm also requires the answered echoes to be at
	// least LinkOKMedianMs slow — loss alone measured the router's ICMP policy,
	// not the radio. See updateLinkLatch.
	LinkBadLoss = 3
	LinkOKLoss  = 1
	// LinkBadMedianMs / LinkOKMedianMs: the RTT arm. A Wi-Fi hop answers in
	// 2-30ms on a clean radio; the 2026-08-27 episode had a median of 28ms
	// with 1055ms peaks and the 2026-08-19 one a median well over 100. Median
	// rather than mean so a single 1s outlier in an otherwise clean minute
	// does not trip it. Bumped the bad threshold from 100 → 200 and leave
	// from 50 → 100 (hysteresis band 100-200) because a median of 101ms on
	// Wi-Fi with 0% loss is not a broken radio — it is a radio with a few
	// transient 200ms peaks that the median smooths over. The old 100ms
	// threshold flapped local-link ↔ healthy every 5s during the 2026-10-04
	// Vultr DNS outage recovery, freezing every leg/ceiling decision.
	LinkBadMedianMs = 200
	LinkOKMedianMs  = 100
)

// LinkSample is one echo to the default gateway. Ms is 0 when it was lost.
type LinkSample struct {
	Ms int  `json:"ms"`
	OK bool `json:"ok"`
}

// RecordLink appends one gateway echo and re-evaluates the latch.
func (s *State) RecordLink(ms int, ok bool) {
	s.LinkSamples = append(s.LinkSamples, LinkSample{Ms: ms, OK: ok})
	if n := len(s.LinkSamples); n > LinkWindow {
		s.LinkSamples = append([]LinkSample(nil), s.LinkSamples[n-LinkWindow:]...)
	}
	s.LinkLastAt = time.Now()
	s.updateLinkLatch()
}

// LinkLoss counts the lost echoes in the window.
func (s *State) LinkLoss() int {
	lost := 0
	for _, l := range s.LinkSamples {
		if !l.OK {
			lost++
		}
	}
	return lost
}

// LinkMedianMs is the median RTT of the answered echoes, 0 when none answered.
func (s *State) LinkMedianMs() int {
	var ms []int
	for _, l := range s.LinkSamples {
		if l.OK {
			ms = append(ms, l.Ms)
		}
	}
	if len(ms) == 0 {
		return 0
	}
	sort.Ints(ms)
	return ms[len(ms)/2]
}

// updateLinkLatch applies the Schmitt trigger. Nothing is decided until the
// window is full, so the first minute after start cannot paint the link bad
// off three samples.
func (s *State) updateLinkLatch() {
	if len(s.LinkSamples) < LinkWindow {
		return
	}
	loss, med := s.LinkLoss(), s.LinkMedianMs()
	// The loss arm needs the RTT arm's corroboration. Three lost echoes in a
	// minute means something only if the ones that DID answer show the radio
	// straining; a router that answers in 3ms and drops a quarter of the echoes
	// is de-prioritising ICMP on a busy channel, not failing to carry traffic.
	// Measured 2026-10-14: nine latches, every one `lost 3/12, median 3ms`, each
	// freezing every leg and ceiling decision for 40-60s. The RTT arm alone
	// (med >= LinkBadMedianMs) still catches a genuinely slow radio.
	switch {
	case med >= LinkBadMedianMs || (loss >= LinkBadLoss && med >= LinkOKMedianMs):
		if !s.LinkBad {
			s.LinkBadSince = time.Now()
		}
		s.LinkBad = true
	case loss <= LinkOKLoss && med < LinkOKMedianMs:
		s.LinkBad = false
		s.LinkBadSince = time.Time{}
	}
}

// ResetLink empties the window, for when the uplink itself changed: echoes to
// the previous network's gateway say nothing about this one.
func (s *State) ResetLink() {
	s.LinkSamples = nil
	s.LinkBad = false
	s.LinkBadSince = time.Time{}
}
