package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type LegEgressTarget struct {
	Host, Path           string
	Status               int
	RequireBody          bool
	RealityTag, Hy2Tag   string
	RealityPort, Hy2Port int
}

// Fixed loopback forwarders cannot be borrowed as general-purpose proxies.
func LegEgressTargets() []LegEgressTarget {
	return []LegEgressTarget{
		{LegEgressHost, "/cdn-cgi/trace", http.StatusOK, true, RealityEgressTag, Hy2EgressTag, RealityEgressPort, Hy2EgressPort},
		{"www.gstatic.com", "/generate_204", http.StatusNoContent, false, "egress-reality-alt-in", "egress-hy2-alt-in", 8102, 8103},
	}
}

func raceEgress(parent context.Context, targets []LegEgressTarget, probe func(context.Context, LegEgressTarget) (time.Duration, error)) (time.Duration, error) {
	if err := parent.Err(); err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, fmt.Errorf("no egress targets")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		delay time.Duration
		err   error
	}
	results := make(chan result, len(targets))
	for _, target := range targets {
		go func(target LegEgressTarget) {
			delay, err := probe(ctx, target)
			if err != nil {
				err = fmt.Errorf("%s: %w", target.Host, err)
			}
			results <- result{delay, err}
		}(target)
	}
	var failures []error
	var worst time.Duration
	for range targets {
		select {
		case <-ctx.Done():
			return worst, ctx.Err()
		case r := <-results:
			if err := parent.Err(); err != nil {
				return worst, err
			}
			if r.err == nil {
				return r.delay, nil
			}
			failures = append(failures, r.err)
			if r.delay > worst {
				worst = r.delay
			}
		}
	}
	return worst, errors.Join(failures...)
}

func probeEgressTarget(ctx context.Context, keeperPort int, target LegEgressTarget, timeout time.Duration) (time.Duration, error) {
	port := target.RealityPort
	if keeperPort == LegGatePort {
		port = target.Hy2Port
	}
	client := legEgressClient(fmt.Sprintf("127.0.0.1:%d", port), timeout)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target.Host+target.Path, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer resp.Body.Close()
	err = validateEgressResponse(resp, target)
	return time.Since(start), err
}

func validateEgressResponse(resp *http.Response, target LegEgressTarget) error {
	if resp.StatusCode != target.Status {
		return fmt.Errorf("http %d, want %d", resp.StatusCode, target.Status)
	}
	const limit = 1 << 20
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("read external body: %w", err)
	}
	if n > limit {
		return fmt.Errorf("external body exceeds 1 MiB")
	}
	if target.RequireBody && n == 0 {
		return fmt.Errorf("external response carried no body")
	}
	if target.Status == http.StatusNoContent && n != 0 {
		return fmt.Errorf("204 response unexpectedly carried a body")
	}
	return nil
}
