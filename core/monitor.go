package core

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

func rejectMonitorRedirect(*http.Request, []*http.Request) error {
	return fmt.Errorf("monitor target redirected away from its fixed origin")
}

// Both monitoring arms use the same independently validated HTTPS targets.
// One origin's outage cannot convict the transport. The shared deadline bounds
// the whole check, and the losing request is canceled without delaying a result.
func (s *Supervisor) fetchMonitor(client *http.Client) error {
	timeout := time.Duration(s.probeTimeMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := raceEgress(ctx, LegEgressTargets(), func(ctx context.Context, target LegEgressTarget) (time.Duration, error) {
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
	})
	return err
}

// A candidate must prove authenticated delivery before a recovery is committed.
// The selector write happens in handoffToPrimary before evidence is discarded.
func (s *Supervisor) recoverPrimary() {
	if !s.hasReality || !s.onFallback() || s.linkBad() || utils.ReadLeg() != utils.LegAuto {
		return
	}
	if err := verifyLegRecovery(func() (time.Duration, error) {
		return s.probeLeg(BrutalKeeperPort)
	}, s.waitLegProbe); err != nil {
		log.Printf("fallback: REALITY delivery not recovered (%v); retaining HY2 and its failure evidence", err)
		return
	}
	s.handoffToPrimary("unhealthy HY2 fallback; REALITY passed three spaced authenticated delivery checks")
}
