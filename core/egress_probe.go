package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	Hy2ProbeSelectorTag = "probe-hy2"
	RealityEgressTag    = "egress-reality-in"
	Hy2EgressTag        = "egress-hy2-in"
	RealityEgressPort   = 8100
	Hy2EgressPort       = 8101
	LegEgressHost       = "cloudflare.com"
)

// Each local forwarder reaches one fixed external TLS host over its own leg.
// Keep the original URL so TLS still verifies the external name. Environment
// proxies and connection reuse must not move the request onto a different leg.
func legEgressClient(forwardAddr string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", forwardAddr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("external probe redirected away from its fixed target")
		},
	}
}

func LegEgressProbe(keeperPort, timeoutMs int) (time.Duration, error) {
	if keeperPort != BrutalKeeperPort && keeperPort != LegGatePort {
		return 0, fmt.Errorf("unknown keeper port %d", keeperPort)
	}
	if timeoutMs <= 0 {
		return 0, fmt.Errorf("invalid egress timeout")
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return raceEgress(ctx, LegEgressTargets(), func(ctx context.Context, target LegEgressTarget) (time.Duration, error) {
		return probeEgressTarget(ctx, keeperPort, target, timeout)
	})
}
