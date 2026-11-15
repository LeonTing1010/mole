package utils

import "time"

// Allocation is for new connections; established flows keep their transport.
type BalanceStatus struct {
	Supported          bool                `json:"supported"`
	Enabled            bool                `json:"enabled"`
	Sampling           bool                `json:"sampling,omitempty"`
	RealityLanes       int                 `json:"reality_lanes"`
	Hy2Lanes           int                 `json:"hy2_lanes"`
	RealityHealthy     bool                `json:"reality_healthy"`
	Hy2Healthy         bool                `json:"hy2_healthy"`
	RealityDegraded    bool                `json:"reality_degraded,omitempty"`
	Hy2Degraded        bool                `json:"hy2_degraded,omitempty"`
	RealityLatencyMs   int64               `json:"reality_latency_ms,omitempty"`
	Hy2LatencyMs       int64               `json:"hy2_latency_ms,omitempty"`
	LastSampleAt       time.Time           `json:"last_sample_at,omitempty"`
	AllocationMbps     []float64           `json:"allocation_mbps,omitempty"` // legacy nine-way scans
	AllocationEvidence *AllocationEvidence `json:"allocation_evidence,omitempty"`
	SampleError        string              `json:"sample_error,omitempty"`
	LastCheckedAt      time.Time           `json:"last_checked_at,omitempty"`
	RealityError       string              `json:"reality_error,omitempty"`
	Hy2Error           string              `json:"hy2_error,omitempty"`
	Error              string              `json:"error,omitempty"`
	Hy2DownMbps        int                 `json:"hy2_down_mbps,omitempty"`
	Hy2UpMbps          int                 `json:"hy2_up_mbps,omitempty"`
	UDPPressureCapMbps int                 `json:"udp_pressure_cap_mbps,omitempty"`
	UDPCooldownUntil   time.Time           `json:"udp_cooldown_until,omitempty"`
}

// AllocationEvidence describes synthetic keeper comparisons, not user traffic.
type AllocationEvidence struct {
	CurrentHy2Lanes      int       `json:"current_hy2_lanes"`
	CandidateHy2Lanes    int       `json:"candidate_hy2_lanes"`
	FlowConnections      []int     `json:"flow_connections,omitempty"`
	CandidateOnEdges     bool      `json:"candidate_on_edges"`
	RatesMbps            []float64 `json:"rates_mbps"`
	HeaderMedianMs       []int64   `json:"header_median_ms"`
	LoadedGatewayWorstMs []int64   `json:"loaded_gateway_worst_ms"`
	GainPct              float64   `json:"gain_pct"`
	Qualified            bool      `json:"qualified"`
	Decision             string    `json:"decision"`
	ConsecutiveWins      int       `json:"consecutive_wins"`
	ProbePayloadBytes    int64     `json:"probe_payload_bytes"`
}
