package cmd

import (
	"fmt"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
)

// Post-deploy verification.
//
// A boot script runs unattended on a box nobody can log into, and the deploy
// used to end the moment Vultr handed back an IP — which is where the
// 2026-09-04 node was declared done: sing-box lost a race for the dpkg lock,
// TCP/443 refused for seventeen hours, and the first thing to notice was a
// person, the next day. The only honest end of a deploy is both legs
// answering from outside the tunnel, so that is what this waits for.
const (
	nodeVerifyBudget = 6 * time.Minute
	nodeVerifyEvery  = 5 * time.Second
	nodeProbeTimeout = 3 * time.Second
)

// legReport is what one leg looked like at the end of the wait.
type legReport struct {
	Verdict utils.ProbeVerdict
	RTT     time.Duration
	Err     error
}

func (r legReport) ok() bool { return r.Verdict == utils.ProbeAlive }

// verifyNodeLegs probes both legs from the physical interface until both
// answer or the budget runs out, printing each change as it happens. It does
// not fail the deploy — the instance exists and is recorded either way — but
// it says exactly which leg is missing and what to do about it.
func verifyNodeLegs(srv Server, budget time.Duration) (hy2, reality legReport) {
	// Our own kill switch would otherwise drop the probes: the node is not
	// yet the active server, so its address is not in the pass table.
	_ = utils.AllowDirectHost(srv.IP)

	hy2Addr := fmt.Sprintf("%s:%d", srv.IP, srv.Port)
	realityAddr := srv.realityAddr()
	wantReality := realityAddr != ""
	wantHy2 := !srv.isReality()

	fmt.Printf("🔎 Verifying the node from outside the tunnel (up to %s)...\n", budget)
	deadline := time.Now().Add(budget)
	var lastHy2, lastReality utils.ProbeVerdict
	for {
		if wantHy2 {
			rtt, v, err := core.ProbeHy2UDP(hy2Addr, srv.ObfsPassword, nodeProbeTimeout)
			hy2 = legReport{Verdict: v, RTT: rtt, Err: err}
			if v != lastHy2 {
				lastHy2 = v
				fmt.Printf("   hysteria2 UDP/%d: %s\n", srv.Port, describeLeg(hy2))
			}
		}
		if wantReality {
			rtt, v, err := core.ProbeTCP(realityAddr, nodeProbeTimeout)
			reality = legReport{Verdict: v, RTT: rtt, Err: err}
			if v != lastReality {
				lastReality = v
				fmt.Printf("   REALITY  TCP/%s: %s\n", portOf(realityAddr), describeLeg(reality))
			}
		}
		if (!wantHy2 || hy2.ok()) && (!wantReality || reality.ok()) {
			fmt.Println("✅ Both legs answer — the node is ready.")
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(nodeVerifyEvery)
	}
	fmt.Println("⚠️  The node did not come up cleanly:")
	if wantHy2 && !hy2.ok() {
		fmt.Printf("   hysteria2 UDP/%d is %s — %s\n", srv.Port, hy2.Verdict, adviseLeg(hy2, "hysteria"))
	}
	if wantReality && !reality.ok() {
		fmt.Printf("   REALITY TCP/%s is %s — %s\n", portOf(realityAddr), reality.Verdict, adviseLeg(reality, "sing-box"))
	}
	return
}

func describeLeg(r legReport) string {
	switch r.Verdict {
	case utils.ProbeAlive:
		return fmt.Sprintf("answers (%dms)", int(r.RTT/time.Millisecond))
	case utils.ProbeRefused:
		return "refused — the box is up, nothing is listening yet"
	case utils.ProbeSilent:
		return "silent — no answer yet"
	default:
		if r.Err != nil {
			return fmt.Sprintf("%s (%v)", r.Verdict, r.Err)
		}
		return string(r.Verdict)
	}
}

// adviseLeg turns the final verdict into the one thing to do next.
func adviseLeg(r legReport, unit string) string {
	switch r.Verdict {
	case utils.ProbeRefused:
		return fmt.Sprintf("the box answers but %s is not listening: the boot script failed on it. `mole server repair` reinstalls it over SSH", unit)
	case utils.ProbeSilent:
		return "nothing answers: either the boot script is still running, the cloud firewall is not attached, or the path is dark. Try `mole server repair` in a minute; if the probe stays silent while the box is reachable it is the firewall group"
	case utils.ProbeNoLink:
		return "this Mac has no uplink right now; verify later with `mole server repair --check`"
	default:
		if r.Err != nil {
			return r.Err.Error()
		}
		return "unknown"
	}
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
