package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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

// stuckLeg names the wanted leg that is not answering, in plain words, for the
// degraded-handoff line.
func stuckLeg(srv Server, result nodeVerification) string {
	h, r := wantsLegs(srv)
	if h && !result.HY2.ok() {
		return "hysteria2 (" + plainVerdict(result.HY2) + ")"
	}
	if r && !result.Reality.ok() {
		return "REALITY (" + plainVerdict(result.Reality) + ")"
	}
	return "one leg"
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
		opts.Heartbeat = 30 * time.Second
	}
	started := time.Now()
	result := nodeVerification{Status: "unverified"}
	update := func() {
		result.Status = verificationStatus(srv, result.HY2, result.Reality)
		result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	progress := func() {
		fmt.Fprintf(opts.Writer, "   … %s elapsed — still installing (%s)\n", time.Since(started).Round(time.Second), legSummary(srv, result))
	}
	fmt.Fprintln(opts.Writer, "Checking each protocol from outside the tunnel…")
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
			fmt.Fprintf(opts.Writer, "✅ All requested protocols answered and delivered real HTTPS traffic in %s.\n", time.Since(started).Round(time.Second))
			return result, nil
		}
		// Recovery can investigate a partial node, while its shared node lock
		// prevents writes until the boot installer has finished.
		if result.Status == "degraded" {
			if degradedSince.IsZero() {
				degradedSince = time.Now()
			} else if time.Since(degradedSince) >= degradedGrace {
				fmt.Fprintf(opts.Writer, "   one protocol answered, but %s has been down for %s — not waiting further\n", stuckLeg(srv, result), degradedGrace.Round(time.Second))
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
	if w != nil && tuning.Status != "unobserved" {
		switch tuning.Status {
		case "ready":
			fmt.Fprintln(w, "Network tuning: applied.")
		case "degraded":
			fmt.Fprintln(w, "Network tuning: only partly applied — see /var/log/mole/boot.log on the node.")
		default:
			fmt.Fprintf(w, "Network tuning: %s\n", tuning.Status)
		}
	}
	return result, verifyErr
}

func describeLeg(r legReport) string {
	switch r.Verdict {
	case utils.ProbeAlive:
		return fmt.Sprintf("answers in %dms", int(r.RTT/time.Millisecond))
	case utils.ProbeRefused:
		return "port open, protocol not ready yet"
	case utils.ProbeSilent:
		return "no answer yet"
	case utils.ProbeNoLink:
		return "no uplink on this Mac"
	default:
		if r.Err != nil {
			return fmt.Sprintf("%s (%v)", r.Verdict, r.Err)
		}
		return string(r.Verdict)
	}
}

// plainVerdict is describeLeg without the timing, for one-line summaries.
func plainVerdict(r legReport) string {
	switch r.Verdict {
	case utils.ProbeAlive:
		return "answered"
	case utils.ProbeRefused:
		return "port open, not ready"
	case utils.ProbeSilent:
		return "no answer"
	case utils.ProbeNoLink:
		return "no uplink"
	default:
		return string(r.Verdict)
	}
}

// legSummary names only the legs this deploy actually asked for, so a
// hysteria2-only node does not report on a standby it never requested.
func legSummary(srv Server, result nodeVerification) string {
	h, r := wantsLegs(srv)
	var parts []string
	if h {
		parts = append(parts, "hy2 "+plainVerdict(result.HY2))
	}
	if r {
		parts = append(parts, "reality "+plainVerdict(result.Reality))
	}
	return strings.Join(parts, ", ")
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
