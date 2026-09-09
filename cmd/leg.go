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
	Use:   "leg [hy2|reality|auto]",
	Short: "Pin the transport leg (UDP hysteria2 or TCP VLESS+REALITY) without reconnecting",
	Long: `Hold the tunnel on one transport, or hand the choice back with "auto".

Both legs run on the same node and are members of the same selector, so
switching costs one Clash-API call: no reconnect, no rebuilt TUN, no dropped
connections. What differs is the protocol underneath.

  reality   VLESS+REALITY over TCP/443. The PRIMARY leg. Looks like ordinary
            HTTPS to the path, has never been seen throttled, and since
            2026-09-06 the node paces it with TCP Brutal at the ceiling rung,
            so it holds its rate through loss the way hy2 does (paired 8 MB
            samples on a 5%-loss evening: Brutal 24 Mbps median / 13 floor
            against BBR's 19 / 3.5).
  hy2       hysteria2 over UDP/443, the Brutal ceiling ladder. The FALLBACK:
            what mole moves to when REALITY is measured dark, and the one leg
            that holds its rate through loss. Also the leg the path's UDP QoS
            breaks ten times a day — new connections fail while established
            ones and single datagrams are fine — and sustained uploads on it
            truncate at 2.75 MiB.
  auto      Let mole choose. It stays on REALITY and falls back to the hy2
            ladder when it can measure that TCP/443 specifically has stopped
            carrying; it returns after a dwell that doubles on a premature return.

"auto" is right almost always. Reach for a pin when you are measuring one leg
against the other and need the daemon to stop switching underneath you, or when
you know something about tonight the health signals do not.`,
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
		return fmt.Errorf("expected \"hy2\", \"reality\" or \"auto\", got %q", args[0])
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

	pid, perr := utils.ReadPID()
	running := perr == nil && utils.IsRunning(pid)
	if !running {
		if want == utils.LegAuto {
			fmt.Println("Leg → auto — mole will choose when it starts.")
		} else {
			fmt.Printf("Leg pinned to %s — will apply when mole starts.\n", want)
		}
		return nil
	}

	// Clearing the pin deliberately does NOT select anything. The right leg
	// under "auto" depends on failover state that lives in the daemon's memory
	// — whether it is mid-dwell on REALITY, and at what backoff — and guessing
	// here would yank traffic onto a leg the supervisor undoes a tick later.
	if want == utils.LegAuto {
		fmt.Println("✔ Leg → auto — mole chooses again; it reconciles within a minute.")
		return nil
	}

	clash := core.NewClashClient("127.0.0.1:9090")
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
		if now == core.RealityLegTag {
			fmt.Println("Live:    VLESS+REALITY (TCP/443)")
		} else {
			fmt.Printf("Live:    hysteria2 (UDP/443) via %s\n", now)
		}
	}

	// The number that justifies a switch, so the decision is not taken blind.
	// See utils.DialSamples: the fresh-dial arm is the only instrument that
	// sees the failure REALITY answers.
	if st, err := utils.ReadState(); err == nil && len(st.DialSamples) > 0 {
		bad := st.DialBad()
		verdict := "new connections opening normally"
		if st.SlowNewFlows {
			verdict = "NEW CONNECTIONS FAILING on the active leg — mole is comparing against the idle one"
		}
		fmt.Printf("Dials:   %d/%d recent new connections slow or failed — %s\n", bad, len(st.DialSamples), verdict)
	}
	if srv.StandbyURI() == "" {
		fmt.Printf("         %s has no REALITY leg; redeploy to get one.\n", srv.Name)
	}
	fmt.Println("         mole leg reality   hold TCP (the primary; disarms the fallback)")
	fmt.Println("         mole leg hy2       hold UDP (Brutal ladder; bypasses the primary)")
	fmt.Println("         mole leg auto      back to automatic")
}
