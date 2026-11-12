package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

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
	// A partial node may hand off to recovery early. Recovery must wait for the
	// boot/repair lock; elapsed time alone never proves boot installation ended.
	degradedGrace = 90 * time.Second
)

// legReport is what one leg looked like at the end of the wait.
type legReport struct {
	Verdict utils.ProbeVerdict `json:"verdict"`
	RTT     time.Duration      `json:"rtt_ns"`
	Err     error              `json:"-"`
	Detail  string             `json:"detail,omitempty"`
}

func (r legReport) ok() bool { return r.Verdict == utils.ProbeAlive }

// verifyNodeLegs is retained for repair callers; deployment uses verifyNode
// and propagates the returned error instead of treating a timeout as success.
func verifyNodeLegs(srv Server, budget time.Duration) (legReport, legReport) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	result, _ := verifyNode(ctx, srv, os.Stdout)
	return result.HY2, result.Reality
}

type nodeVerification struct {
	Status    string            `json:"status"`
	Tuning    *nodeTuningReport `json:"tuning,omitempty"`
	CheckedAt string            `json:"checked_at"`
	HY2       legReport         `json:"hy2"`
	Reality   legReport         `json:"reality"`
}

type nodeVerifyOptions struct {
	Probe     func(context.Context, string) legReport
	Writer    io.Writer
	Every     time.Duration
	Heartbeat time.Duration
}

func wantsLegs(srv Server) (bool, bool) {
	return !srv.isReality(), srv.isReality() || srv.hasStandbyLeg()
}

func verificationStatus(srv Server, hy2, reality legReport) string {
	h, r := wantsLegs(srv)
	if (!h || hy2.ok()) && (!r || reality.ok()) {
		return "ready"
	}
	if (h && hy2.ok()) || (r && reality.ok()) {
		return "degraded"
	}
	return "unverified"
}

// stuckLegVerdict returns the verdict of the wanted leg that is not alive, for
// the degraded-state progress message.
func stuckLegVerdict(srv Server, hy2, reality legReport) string {
	h, r := wantsLegs(srv)
	if h && !hy2.ok() {
		return string(hy2.Verdict)
	}
	if r && !reality.ok() {
		return string(reality.Verdict)
	}
	return "unknown"
}

// verifyNodeWith owns the deadline and output. Probes represent completed
// authenticated requests; transport-only reachability cannot produce ready.
func verifyNodeWith(ctx context.Context, srv Server, opts nodeVerifyOptions) (nodeVerification, error) {
	if opts.Writer == nil {
		opts.Writer = io.Discard
	}
	if opts.Every <= 0 {
		opts.Every = nodeVerifyEvery
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 15 * time.Second
	}
	started := time.Now()
	result := nodeVerification{Status: "unverified"}
	update := func() {
		result.Status = verificationStatus(srv, result.HY2, result.Reality)
		result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	progress := func() {
		remaining := "unbounded"
		if d, ok := ctx.Deadline(); ok {
			remaining = max(time.Duration(0), time.Until(d)).Round(time.Second).String()
		}
		fmt.Fprintf(opts.Writer, "elapsed=%s remaining=%s HY2=%s REALITY=%s (remote installation stage unobserved)\n", time.Since(started).Round(time.Second), remaining, result.HY2.Verdict, result.Reality.Verdict)
	}
	fmt.Fprintln(opts.Writer, "Verifying authenticated protocol requests from outside the tunnel...")
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()
	type probeResult struct {
		leg    string
		report legReport
	}
	h, r := wantsLegs(srv)
	var degradedSince time.Time
	for {
		if ctx.Err() != nil {
			update()
			return result, fmt.Errorf("node %s: %s: %w", srv.Name, result.Status, ctx.Err())
		}
		replies := make(chan probeResult, 2)
		pending := 0
		for _, leg := range []string{"hy2", "reality"} {
			if (leg == "hy2" && !h) || (leg == "reality" && !r) {
				continue
			}
			pending++
			go func(leg string) { replies <- probeResult{leg, opts.Probe(ctx, leg)} }(leg)
		}
		for pending > 0 {
			select {
			case <-ctx.Done():
				update()
				return result, fmt.Errorf("node %s: %s: %w", srv.Name, result.Status, ctx.Err())
			case <-heartbeat.C:
				progress()
			case reply := <-replies:
				pending--
				old := result.HY2
				if reply.leg == "reality" {
					old = result.Reality
					result.Reality = reply.report
				} else {
					result.HY2 = reply.report
				}
				if old.Verdict != reply.report.Verdict {
					fmt.Fprintf(opts.Writer, "   %s: %s\n", reply.leg, describeLeg(reply.report))
				}
			}
		}
		update()
		if result.Status == "ready" {
			fmt.Fprintln(opts.Writer, "All requested protocols authenticated and delivered HTTPS payloads.")
			return result, nil
		}
		// Recovery can investigate a partial node, while its shared node lock
		// prevents writes until the boot installer has finished.
		if result.Status == "degraded" {
			if degradedSince.IsZero() {
				degradedSince = time.Now()
			} else if time.Since(degradedSince) >= degradedGrace {
				fmt.Fprintf(opts.Writer, "   one leg up, the other stuck at %s for %s — stopping early for repair\n", stuckLegVerdict(srv, result.HY2, result.Reality), degradedGrace)
				return result, fmt.Errorf("node %s: %s: one leg has been up while the other stayed down for %s", srv.Name, result.Status, degradedGrace)
			}
		} else {
			degradedSince = time.Time{}
		}
		timer := time.NewTimer(opts.Every)
	waiting:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, fmt.Errorf("node %s: %s: %w", srv.Name, result.Status, ctx.Err())
			case <-heartbeat.C:
				progress()
			case <-timer.C:
				break waiting
			}
		}
	}
}

func verifyNode(ctx context.Context, srv Server, w io.Writer) (nodeVerification, error) {
	empty := nodeVerification{Status: "unverified", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := utils.AllowDirectHostContext(ctx, serverHost(srv)); err != nil {
		return empty, fmt.Errorf("direct probe exemption unavailable: %w", err)
	}
	helper, err := newProtocolProbe(ctx, srv)
	if err != nil {
		return empty, err
	}
	defer helper.Close()
	result, verifyErr := verifyNodeWith(ctx, srv, nodeVerifyOptions{Writer: w, Probe: helper.Probe})
	tuning := helper.Tuning(ctx)
	result.Tuning = &tuning
	if w != nil {
		fmt.Fprintf(w, "Network tuning: %s (protocol readiness: %s)\n", tuning.Status, result.Status)
	}
	return result, verifyErr
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
		return fmt.Sprintf("the endpoint refuses connections on %s; installation may still be in progress. `mole server repair` reinstalls it over SSH", unit)
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
