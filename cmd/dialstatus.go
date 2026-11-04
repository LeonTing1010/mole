package cmd

import (
	"fmt"
	"github.com/LeonTing1010/mole/utils"
)

func dialStatus(st *utils.State) string {
	bad, n := st.DialBad(), len(st.DialSamples)
	fails := st.DialFails()
	verdict := "probe requests completing normally"
	switch {
	case st.LinkBad:
		verdict = "sampling paused while local link is bad"
	case st.SlowNewFlows:
		verdict = "probe window degraded; slow replies are not failures"
	case n < utils.DialWindow:
		verdict = fmt.Sprintf("collecting samples (%d/%d)", n, utils.DialWindow)
	case bad > 0:
		verdict = "slow replies or failures observed; failover uses failed probes"
	}
	freshness := "last sample time unavailable"
	if !st.LastDialAt.IsZero() {
		freshness = "last sample " + st.LastDialAt.Local().Format("2006-01-02 15:04:05")
	}
	if st.ApplicationNodeDialFailures > 0 {
		freshness += fmt.Sprintf("; %d application node-dial failures observed since start, last %s", st.ApplicationNodeDialFailures, st.LastApplicationNodeDialFailureAt.Local().Format("15:04:05"))
	}
	return fmt.Sprintf("%d/%d probe requests slow or failed (%d slow >%dms, %d failed; %s) — %s", bad, n, bad-fails, utils.DialSlowMs, fails, freshness, verdict)
}
