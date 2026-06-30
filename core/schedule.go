package core

import "time"

// Outbound tags used by the time-of-day bandwidth scheme. When a peak profile is
// configured, config.Build emits two hy2 outbounds (peak/off-peak) behind a
// selector tagged ProxySelectorTag; the route still targets that selector, and
// the supervisor flips its selected member via the Clash API. Without a peak
// profile the proxy is a single outbound tagged ProxySelectorTag, so routing and
// the Clash keepalive reference the same tag either way.
const (
	ProxySelectorTag = "proxy"
	ProxyOffpeakTag  = "proxy-offpeak"
	ProxyPeakTag     = "proxy-peak"
)

// BandwidthSchedule encodes a time-of-day Brutal ceiling for the hy2 outbound.
//
// Why this exists: hysteria2's Brutal congestion control holds a FIXED declared
// send rate and ignores packet loss by design. On a cross-border path whose
// available bandwidth swings by time of day (e.g. CN→NRT measured ~30 Mbps
// off-peak but ~2.5 Mbps at peak), no single fixed ceiling fits — set it high
// and Brutal floods the throttled path at peak (ENOBUFS + retransmit storms +
// "no recent network activity" stalls); set it low and off-peak throughput is
// needlessly capped. BBR (no declared bandwidth) is not the answer either: it is
// loss-sensitive and collapses to tens of KB/s on this lossy path at peak.
//
// The fix is to track the link: run a low ceiling during the peak window and a
// high one off-peak. The supervisor selects the matching outbound on a timer.
type BandwidthSchedule struct {
	OffUp, OffDown   int // off-peak ceiling (the server's normal up_mbps/down_mbps)
	PeakUp, PeakDown int // peak-window ceiling
	StartHour        int // local-clock peak window start hour [0,24)
	EndHour          int // local-clock peak window end hour [0,24); wraps past midnight
}

// Enabled reports whether a peak profile is configured. With no peak ceiling the
// schedule is inert and the supervisor never flips — behaviour is identical to a
// plain fixed-bandwidth server.
func (s BandwidthSchedule) Enabled() bool { return s.PeakDown > 0 }

// inPeak reports whether local time t falls inside the peak window. A window
// where StartHour > EndHour wraps past midnight (e.g. 12→2 means 12:00–02:00).
func (s BandwidthSchedule) inPeak(t time.Time) bool {
	if s.StartHour == s.EndHour {
		return false
	}
	h := t.Hour()
	if s.StartHour < s.EndHour {
		return h >= s.StartHour && h < s.EndHour
	}
	return h >= s.StartHour || h < s.EndHour
}

// Profile returns the profile name and the (up, down) Brutal ceiling that apply
// at local time t. When the schedule is disabled it always returns the off-peak
// ceiling so callers can use it unconditionally.
func (s BandwidthSchedule) Profile(t time.Time) (name string, up, down int) {
	if s.Enabled() && s.inPeak(t) {
		return "peak", s.PeakUp, s.PeakDown
	}
	return "offpeak", s.OffUp, s.OffDown
}

// Member returns the selector member tag to route through at local time t.
func (s BandwidthSchedule) Member(t time.Time) string {
	if s.Enabled() && s.inPeak(t) {
		return ProxyPeakTag
	}
	return ProxyOffpeakTag
}
