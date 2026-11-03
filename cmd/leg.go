package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var legCmd = &cobra.Command{
	Use:   "leg [hy2|reality|both|auto]",
	Short: "Choose one transport or use both for separate connections",
	Long: `Choose how new connections use the two protocols on the same node.

  reality   Hold VLESS+REALITY over TCP/443.
  hy2       Hold the hysteria2 bandwidth ladder over UDP/443.
  auto      Use REALITY as primary with measured fallback and recovery.
  both      Assign source-port buckets using health checks. When both legs
            qualify, the fixed split is 6 TCP / 2 UDP buckets; failures and
            recovery change eligibility, not measured throughput weights.

Existing connections keep their original transport when the mode changes.
A single connection uses one protocol. The two protocols share the same node
and uplink, so their configured ceilings do not add up to guaranteed bandwidth.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLeg,
}

func init() { rootCmd.AddCommand(legCmd) }

func runLeg(_ *cobra.Command, args []string) error {
	srv, err := ActiveServer()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		printLeg(srv)
		return nil
	}

	// Resolve everything that can fail before writing anything, so a typo or a
	// node without a standby leg leaves both the pin and the live selector
	// exactly as they were.
	arg := strings.ToLower(strings.TrimSpace(args[0]))
	var target, want string
	switch arg {
	case "both":
		if srv.StandbyURI() == "" {
			return fmt.Errorf("server %q does not have both transport legs", srv.Name)
		}
		want = utils.LegBoth
	case "auto":
		want = utils.LegAuto
	case "reality", "tcp":
		if srv.StandbyURI() == "" {
			return fmt.Errorf("server %q has no REALITY leg — it was deployed before both legs became standard; redeploy with \"mole server deploy\" to get one", srv.Name)
		}
		want, target = utils.LegReality, core.RealityLegTag
	case "hy2", "hysteria2", "udp":
		want = utils.LegHy2
		target = core.BandwidthRungTag(currentRung(srv))
	default:
		return fmt.Errorf("expected \"hy2\", \"reality\", \"both\" or \"auto\", got %q", args[0])
	}
	pid, perr := utils.ReadPID()
	running := perr == nil && utils.IsRunning(pid)
	supported := false
	if st, err := utils.ReadState(); err == nil {
		supported = st.Balance != nil && st.Balance.Supported
	}
	if want == utils.LegBoth && running {
		if !supported {
			return fmt.Errorf("the running daemon does not support dual routing; restart with this version using `mole down` then `mole up`, then run `mole leg both`")
		}
		if _, err := core.NewClashClient("127.0.0.1:9090").Selection(core.BalanceLaneTag(0)); err != nil {
			return fmt.Errorf("dual flow selectors are not loaded: %w", err)
		}
	}

	// Persist the intent first, for the same reason `mole ceiling` does: if the
	// Clash call below fails because the daemon is down, the pin still applies
	// the moment the supervisor next ticks, and the two can never disagree
	// about what was asked for.
	if want == utils.LegAuto {
		if err := utils.ClearLeg(); err != nil {
			return fmt.Errorf("clear leg pin: %w", err)
		}
	} else if err := utils.WriteLeg(want); err != nil {
		return fmt.Errorf("write leg pin: %w", err)
	}

	if !running {
		if want == utils.LegAuto {
			fmt.Println("Leg → auto — mole will choose when it starts.")
		} else {
			fmt.Printf("Leg pinned to %s — will apply when mole starts.\n", want)
		}
		return nil
	}
	clash := core.NewClashClient("127.0.0.1:9090")
	if want == utils.LegBoth {
		fmt.Println("✔ Dual routing enabled — health-gated source-port buckets (normally 6 TCP / 2 UDP).")
		fmt.Println("   Existing connections keep their transport. Check allocation with `mole leg` or `mole status`.")
		return nil
	}
	if supported {
		if _, err := core.ApplyBalancedLanes(clash, -1); err != nil {
			return fmt.Errorf("single-mode intent saved; flow-selector reset is pending: %w", err)
		}
	}

	// Clearing the pin deliberately does NOT select anything. The right leg
	// under "auto" depends on failover state that lives in the daemon's memory
	// — whether it is mid-dwell on REALITY, and at what backoff — and guessing
	// here would yank traffic onto a leg the supervisor undoes a tick later.
	if want == utils.LegAuto {
		fmt.Println("✔ Leg → auto — mole chooses again; it reconciles within a minute.")
		return nil
	}

	if _, err := clash.TestDelay(target, "https://www.gstatic.com/generate_204", 5000); err != nil {
		fmt.Printf("   (pre-warm of %s failed: %v — switching anyway)\n", target, err)
	}
	if err := clash.SelectProxy(core.ProxySelectorTag, target); err != nil {
		return fmt.Errorf("select %s: %w (pin is saved; the supervisor will apply it within a minute)", target, err)
	}
	switch want {
	case utils.LegReality:
		fmt.Printf("✔ Leg → VLESS+REALITY over TCP/443 (pinned) via %s\n", target)
		fmt.Println("   The automatic fallback to hy2 is disarmed while this pin is set; \"mole ceiling\" has nothing to act on.")
	case utils.LegHy2:
		fmt.Printf("✔ Leg → hysteria2 over UDP/443 (pinned) via %s\n", target)
		fmt.Println("   The REALITY primary is bypassed and the automatic return to it is disarmed while this pin is set.")
	}
	fmt.Println("   No reconnect — TUN and existing connections untouched.")
	return nil
}

// currentRung is the ladder rung hy2 should return to: the ceiling pin when it
// names a real rung, otherwise whatever the clock asks for. The supervisor's
// self-heal backoff is deliberately not consulted — it lives in daemon memory
// and is re-derived on the next tick anyway.
func currentRung(srv *Server) int {
	sched := srv.Schedule()
	if pin := utils.ReadCeiling(); pin > 0 && sched.HasRung(pin) {
		return pin
	}
	_, _, down := sched.Profile(time.Now())
	return down
}

func printLeg(srv *Server) {
	pin := utils.ReadLeg()
	switch pin {
	case utils.LegBoth:
		fmt.Println("Leg:     both — health-gated source-port buckets over TCP and UDP")
	case utils.LegReality:
		fmt.Println("Leg:     reality (pinned) — VLESS+REALITY over TCP/443; fallback to hy2 disarmed")
	case utils.LegHy2:
		fmt.Println("Leg:     hy2 (pinned) — hysteria2 over UDP/443; the REALITY primary is bypassed")
	default:
		fmt.Println("Leg:     auto — REALITY primary; mole falls back to the hy2 ladder when it measures TCP/443 specifically failing")
	}

	// The live selector is the truth about what traffic is on right now, and it
	// can differ from the pin for a whole minute after `mole leg` on a daemon
	// that has not ticked yet. Print both rather than picking one.
	clash := core.NewClashClient("127.0.0.1:9090")
	if now, err := clash.Selection(core.ProxySelectorTag); err == nil {
		label := "Live:"
		if pin == utils.LegBoth {
			label = "Auxiliary:"
		}
		if now == core.RealityLegTag {
			fmt.Printf("%-10s VLESS+REALITY (TCP/443)\n", label)
		} else {
			fmt.Printf("%-10s hysteria2 (UDP/443) via %s\n", label, now)
		}
	}

	// The number that justifies a switch, so the decision is not taken blind.
	// See utils.DialSamples: the fresh-dial arm is the only instrument that
	// sees the failure REALITY answers.
	if st, err := utils.ReadState(); err == nil {
		printBalance(st)
		fmt.Printf("Dials:   %s\n", dialStatus(st))
	}

	if srv.StandbyURI() == "" {
		fmt.Printf("         %s has no REALITY leg; redeploy to get one.\n", srv.Name)
	}
	fmt.Println("         mole leg reality   hold TCP (the primary; disarms the fallback)")
	fmt.Println("         mole leg hy2       hold UDP (Brutal ladder; bypasses the primary)")
	fmt.Println("         mole leg auto      back to automatic")
	fmt.Println("         mole leg both      use both protocols for separate connections")
}

func printBalance(st *utils.State) {
	if st.Balance == nil || !st.Balance.Enabled {
		return
	}
	b := st.Balance
	fmt.Printf("   Dual:    source-port bucket allocation: Reality %.1f%%, Hysteria2 %.1f%%\n", float64(b.RealityLanes)*100/core.BalanceLanes, float64(b.Hy2Lanes)*100/core.BalanceLanes)
	fmt.Println("            bucket shares are not measured byte or connection shares")
	if !b.LastCheckedAt.IsZero() {
		fmt.Printf("            probes checked: %s; Reality %dms, Hysteria2 %dms\n", b.LastCheckedAt.Local().Format("15:04:05"), b.RealityLatencyMs, b.Hy2LatencyMs)
	}
	if b.RealityError != "" {
		fmt.Printf("            Reality probe error: %s\n", b.RealityError)
	}
	if b.Hy2Error != "" {
		fmt.Printf("            Hysteria2 probe error: %s\n", b.Hy2Error)
	}
	if b.Hy2DownMbps > 0 {
		fmt.Printf("            active HY2 ceiling: down %d / up %d Mbps\n", b.Hy2DownMbps, b.Hy2UpMbps)
	}
	if b.UDPPressureCapMbps > 0 {
		fmt.Printf("            local UDP pressure cap: %d Mbps down; restores gradually after clean health\n", b.UDPPressureCapMbps)
	}
	if time.Now().Before(b.UDPCooldownUntil) {
		fmt.Printf("            UDP cooldown until %s; new connections prefer qualified TCP\n", b.UDPCooldownUntil.Local().Format("15:04:05"))
	}
	if b.RealityDegraded || b.Hy2Degraded {
		fmt.Printf("            temporarily slow: Reality=%t Hysteria2=%t; eligibility follows health hysteresis\n", b.RealityDegraded, b.Hy2Degraded)
	}
	if !b.RealityHealthy || !b.Hy2Healthy {
		fmt.Printf("            qualified: Reality=%t Hysteria2=%t; failed/recovering legs wait for clean probes\n", b.RealityHealthy, b.Hy2Healthy)
	}
	if b.Error != "" {
		fmt.Printf("            allocation reconciliation pending: %s\n", b.Error)
	}
}
