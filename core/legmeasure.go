package core

import (
	"fmt"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// Each leg owns its error: a failed primary must not discard clean standby
// measurements. Keep the worst delay and any failure across all three rounds.
type legMeasurement struct {
	delay time.Duration
	err   error
}

func measureLegs(probe func(int) (time.Duration, error), wait func() error) (hy2, reality legMeasurement, err error) {
	for i := 0; i < legGateProbes; i++ {
		if i > 0 {
			if err := wait(); err != nil {
				return hy2, reality, err
			}
		}
		order := []struct {
			result *legMeasurement
			port   int
		}{
			{&reality, BrutalKeeperPort}, {&hy2, LegGatePort},
		}
		if i%2 == 1 {
			order[0], order[1] = order[1], order[0]
		}
		for _, leg := range order {
			d, perr := probe(leg.port)
			if d > leg.result.delay {
				leg.result.delay = d
			}
			if perr != nil && leg.result.err == nil {
				leg.result.err = perr
			}
		}
	}
	return hy2, reality, nil
}

func legComparison(hy2, reality legMeasurement) bool {
	if hy2.err != nil || hy2.delay > time.Duration(utils.DialSlowMs)*time.Millisecond {
		return false
	}
	if reality.err != nil {
		return true
	}
	return legTestSaysTransport(hy2.delay, reality.delay, nil, utils.DialSlowMs)
}

// Recovery uses the same fresh-dial instrument as the comparative gate, with
// three spaced successes rather than one fortunate moment in a bad stretch.
func verifyLegRecovery(probe func() (time.Duration, error), wait func() error) error {
	for i := 0; i < legGateProbes; i++ {
		if i > 0 {
			if err := wait(); err != nil {
				return err
			}
		}
		d, err := probe()
		if err != nil {
			return err
		}
		if d > time.Duration(utils.DialSlowMs)*time.Millisecond {
			return fmt.Errorf("fresh dial slow: %s", d)
		}
	}
	return nil
}

func (s *Supervisor) waitLegProbe() error {
	select {
	case <-s.stopCh:
		return fmt.Errorf("stopping")
	case <-time.After(legGateSpacing):
		return nil
	}
}

func (s *Supervisor) probeLeg(port int) (time.Duration, error) {
	select {
	case <-s.stopCh:
		return 0, fmt.Errorf("stopping")
	default:
	}
	if s.legProbeFn != nil {
		return s.legProbeFn(port, s.probeTimeMs)
	}
	delay, err := LegProbe(port, s.probeTimeMs)
	if err != nil || !s.verifyLegEgress {
		return delay, err
	}
	egress, err := LegEgressProbe(port, s.probeTimeMs)
	if egress > delay {
		delay = egress
	}
	return delay, err
}
