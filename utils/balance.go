package utils

import "time"

// Allocation is for new connections; established flows keep their transport.
type BalanceStatus struct {
	Supported          bool      `json:"supported"`
	Enabled            bool      `json:"enabled"`
	RealityLanes       int       `json:"reality_lanes"`
	Hy2Lanes           int       `json:"hy2_lanes"`
	RealityHealthy     bool      `json:"reality_healthy"`
	Hy2Healthy         bool      `json:"hy2_healthy"`
	RealityDegraded    bool      `json:"reality_degraded,omitempty"`
	Hy2Degraded        bool      `json:"hy2_degraded,omitempty"`
	RealityLatencyMs   int64     `json:"reality_latency_ms,omitempty"`
	Hy2LatencyMs       int64     `json:"hy2_latency_ms,omitempty"`
	LastSampleAt       time.Time `json:"last_sample_at,omitempty"`
	SampleError        string    `json:"sample_error,omitempty"`
	RealityMbps        float64   `json:"reality_mbps,omitempty"`
	Hy2Mbps            float64   `json:"hy2_mbps,omitempty"`
	LastCheckedAt      time.Time `json:"last_checked_at,omitempty"`
	RealityError       string    `json:"reality_error,omitempty"`
	Hy2Error           string    `json:"hy2_error,omitempty"`
	Error              string    `json:"error,omitempty"`
	Hy2DownMbps        int       `json:"hy2_down_mbps,omitempty"`
	Hy2UpMbps          int       `json:"hy2_up_mbps,omitempty"`
	UDPPressureCapMbps int       `json:"udp_pressure_cap_mbps,omitempty"`
	UDPCooldownUntil   time.Time `json:"udp_cooldown_until,omitempty"`
}
