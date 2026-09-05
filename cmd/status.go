package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VPN status, health, and the active bandwidth ceiling",
	RunE:  runStatus,
}

func runStatus(_ *cobra.Command, _ []string) error {
	pid, err := utils.ReadPID()
	running := err == nil && utils.IsRunning(pid)

	active, _ := ActiveServer()

	if running {
		fmt.Printf("● Running — pid %d\n", pid)
	} else {
		fmt.Println("○ Stopped")
	}

	if active != nil {
		fmt.Printf("   Active:  %s — %s (%s)\n", active.Name, active.Region, active.IP)
	} else {
		fmt.Println("   Active:  none — run `mole server deploy`")
	}

	if utils.KillSwitchLoaded() {
		if running {
			fmt.Println("   Shield:  🛡️ kill switch armed — foreign egress locked to tunnel, CN direct")
		} else {
			fmt.Println("   Shield:  🛡️ kill switch STILL armed from an unclean exit — foreign is blocked; `mole up` or `mole down` to resolve")
		}
	}

	if running {
		printSupervisorState()
	} else if active != nil {
		fmt.Println("   Run `mole up` to connect.")
	}

	// Last, so it is the line left on the screen. `mole status` is where people
	// look when they wonder what mole is costing them, and until 2026-08-14 it
	// was the one command that never mentioned a server billing unattended.
	printLeftoverLidDebt()
	return nil
}

func printSupervisorState() {
	st, err := utils.ReadState()
	if err != nil {
		fmt.Println("   Mode:    (supervisor state unavailable)")
		return
	}
	// The supervisor only reports health; it never reroutes. Surface failures
	// inline so a dead VPS or a blackholed path is visible, but be honest that
	// traffic is still pointed at the proxy (foreign connections will time out,
	// not fail-fast).
	switch st.Mode {
	case utils.ModeProxy:
		switch st.Health() {
		case utils.HealthVPSDown:
			// ICMP port unreachable round-tripped: path fine, nothing listening.
			fmt.Printf("   Mode:    🔴 proxy — hy2 process down on VPS (probe refused): %s\n", st.LastProbeError)
		case utils.HealthLocalLink:
			// The first hop is dropping packets. Say so before anything about
			// the tunnel, because everything about the tunnel is suspect while
			// this holds — and because "your Wi-Fi" is the single most useful
			// thing mole can say on a bad evening.
			fmt.Printf("   Mode:    🟡 proxy — LOCAL LINK BAD: gateway %s lost %d/%d echoes, median %dms, for %s; this is the Wi-Fi, not the tunnel — both legs will be slow and mole is changing nothing until it clears\n",
				st.LinkGateway, st.LinkLoss(), utils.LinkWindow, st.LinkMedianMs(), humanize(time.Since(st.LinkBadSince)))
			if st.LinkPHY != "" {
				fmt.Printf("            radio: %s\n", st.LinkPHY)
			}
		case utils.HealthNoLink:
			// Nothing to say about the VPS: this machine has no uplink. Naming
			// the real culprit matters — reported as a path blackhole, this sent
			// people hunting the tunnel while the Wi-Fi was re-associating.
			fmt.Printf("   Mode:    🔴 proxy — this Mac has no uplink (%s); the tunnel resumes on its own when the network returns\n",
				st.LastProbeError)
		case utils.HealthPathDead:
			// Two flavors, same advice: restarting won't help, it usually
			// clears on its own in ~10–15min.
			if st.ConsecutiveFails >= utils.ProbeSilentThreshold {
				fmt.Printf("   Mode:    🟡 proxy — UDP path to VPS dark (probe %s ×%d, keepalive ×%d); wait it out or `mole down`, restarting won't help\n",
					st.LastProbeVerdict, st.ConsecutiveFails, st.KeepaliveFails)
			} else {
				fmt.Printf("   Mode:    🟡 proxy — VPS answers fresh probes but the established tunnel is dark for %s (keepalive ×%d); a stall kick is dialing a new session\n",
					humanize(time.Since(st.KeepaliveFailingSince)), st.KeepaliveFails)
			}
		case utils.HealthDegraded:
			// Explicitly not an outage in either flavor — the tunnel underneath
			// is passing traffic. Which flavor matters, though, because only one
			// of them is something the user is feeling right now.
			if st.SlowNewFlows {
				// Measured, not inferred: this is the state where an open
				// connection holds ~100ms and full speed while every newly
				// opened one takes seconds. Say what it looks like from a
				// browser, since that is where it gets noticed.
				fmt.Printf("   Mode:    🟡 proxy — established connections fine, but %d of the last %d NEW ones were slow or failed (last %s); pages you already have open are fast, new sites hang\n",
					st.DialBad(), len(st.DialSamples), dialLast(st))
			} else {
				// The probe opens a new five-tuple each time and this path
				// loses ~20% of those first packets.
				fmt.Printf("   Mode:    🟢 proxy — carrying traffic; out-of-tunnel probe missing replies (%s ×%d), new connections may be slow to open\n",
					st.LastProbeVerdict, st.ConsecutiveFails)
			}
		default:
			if st.LastLatencyMs > 0 {
				fmt.Printf("   Mode:    🟢 proxy — VPS healthy (%dms)\n", st.LastLatencyMs)
			} else {
				fmt.Println("   Mode:    🟢 proxy — VPS healthy")
			}
		}
	default:
		fmt.Printf("   Mode:    %s\n", st.Mode)
	}
	// Time-of-day Brutal ceiling, when the active server has a peak profile.
	//
	// The recorded profile is the scheduler's belief, refreshed once a minute;
	// the selector is what is actually carrying traffic. `mole ceiling` moves the
	// selector immediately, so for up to a tick the two legitimately disagree —
	// and reporting the belief as if it were the ceiling is how this line used to
	// claim "↓5 Mbps" while the tunnel ran at 30. Lead with the live number, and
	// name the disagreement instead of hiding it: past the settling window a
	// standing mismatch means the scheduler has lost the selector.
	if st.BandwidthProfile != "" {
		live, onStandby := 0, false
		if now, err := core.NewClashClient("127.0.0.1:9090").Selection(core.ProxySelectorTag); err == nil {
			live = core.BandwidthRungMbps(now)
			onStandby = now == core.RealityLegTag
		}
		switch {
		// The standby leg has no Brutal ceiling to report — it is TCP, so
		// congestion control belongs to the kernel at both ends. Printing "↓0
		// Mbps" here would read as a broken ceiling rather than a deliberate
		// change of transport, so say which leg is carrying traffic instead.
		// Why traffic is on REALITY matters more than that it is. One of these
		// is mole reporting a path problem it worked around; the other is the
		// user's own instruction still in force, and left unsaid it reads as a
		// fault that will not clear.
		case onStandby && utils.ReadLeg() == utils.LegReality:
			fmt.Println("   Profile: 📌 leg pinned — VLESS+REALITY over TCP/443 (fallback to hy2 disarmed; \"mole leg auto\" to release)")
		case onStandby:
			fmt.Println("   Profile: VLESS+REALITY over TCP/443 (primary; hy2 ladder on standby, no Brutal ceiling applies)")
		case strings.HasPrefix(st.BandwidthProfile, "fallback-hy2/") && st.RealityServerDown != "":
			// Not "measured dark": the node itself refused the port. Waiting
			// will not fix this and the line should not suggest it might.
			fmt.Printf("   Profile: 🎭 fallback leg — hysteria2 ladder, Brutal ↓%d Mbps; REALITY server NOT LISTENING on the node (%s) — `mole server repair` reinstalls it\n",
				st.BandwidthDownMbps, st.RealityServerDown)
		case strings.HasPrefix(st.BandwidthProfile, "fallback-hy2/") && (live == 0 || live == st.BandwidthDownMbps):
			fmt.Printf("   Profile: 🎭 fallback leg — hysteria2 ladder, Brutal ↓%d Mbps (REALITY was measured dark; returns on its own)\n", st.BandwidthDownMbps)
		case live == 0 || live == st.BandwidthDownMbps:
			fmt.Printf("   Profile: %s (Brutal ↓%d Mbps)%s\n", st.BandwidthProfile, st.BandwidthDownMbps, legPinNote())
		default:
			fmt.Printf("   Profile: Brutal ↓%d Mbps live — scheduler still on %q (↓%d), reconciles next tick\n",
				live, st.BandwidthProfile, st.BandwidthDownMbps)
		}
	}
	printLidDestroy(st)
	// The first hop, whenever it has been measured. One line, so the reader
	// who sees a yellow tunnel can check the radio without leaving status.
	if !st.LinkLastAt.IsZero() && st.LinkGateway != "" {
		mark := "🟢"
		if st.LinkBad {
			mark = "🔴"
		}
		fmt.Printf("   Link:    %s gateway %s — %d/%d echoes lost, median %dms\n",
			mark, st.LinkGateway, st.LinkLoss(), utils.LinkWindow, st.LinkMedianMs())
	}
	if !st.LastProbeAt.IsZero() {
		fmt.Printf("   Probed:  %s ago\n", humanize(time.Since(st.LastProbeAt)))
	}
	// In-tunnel keepalive. One or two failures are a hiccup (DNS + QUIC ride
	// this path); a streak ≥ threshold already flipped the Mode line above to
	// path-blackhole. Either way, show the streak so trends are visible.
	if !st.LastKeepaliveAt.IsZero() {
		if st.LastKeepaliveError != "" {
			fmt.Printf("   Tunnel:  ⚠️  keepalive failing ×%d (last %s ago): %s\n",
				st.KeepaliveFails, humanize(time.Since(st.LastKeepaliveAt)), st.LastKeepaliveError)
		} else {
			fmt.Printf("   Tunnel:  warm (%dms via proxy, %s ago)\n",
				st.LastKeepaliveMs, humanize(time.Since(st.LastKeepaliveAt)))
		}
	}
}

// printLidDestroy reports what the running daemon will do to the server when
// the lid is shut — or, once the destroy has fired, that it already did.
// Shown on every `mole status` and once at the end of `mole up`, because the
// failure mode this is here to catch is not an error anyone sees — it is a
// feature that stays quiet and a bill that arrives later.
//
// An empty LidDestroy means the daemon predates the field, not that the watcher
// is off; say nothing rather than assert either way.
func printLidDestroy(st *utils.State) {
	switch st.LidDestroy {
	case utils.LidDestroyArmed:
		fmt.Println("   Lid:     🟢 destroy-on-lid-close armed — shutting the lid deletes the server")
	case utils.LidDestroyDryRun:
		fmt.Println("   Lid:     🧪 destroy-on-lid-close DRY RUN — a lid close is logged, nothing is destroyed")
	case utils.LidDestroyFired:
		fmt.Printf("   Lid:     🔴 destroy-on-lid-close FIRED — the server was destroyed (%s)\n", st.LidDestroyReason)
		fmt.Println("            The daemon is stopping; bring the tunnel back with `mole server deploy` then `mole up`")
	case utils.LidDestroyDeferred:
		fmt.Printf("   Lid:     🟡 the lid shut at %s — server kept for the grace period, destroyed on the first wake after it if the lid is still shut\n",
			st.LidDestroyPendingSince.Format("15:04"))
	case utils.LidDestroyPending:
		fmt.Printf("   Lid:     🟠 destroy-on-lid-close FAILED at %s (%d attempts) — the server is STILL BILLING, %s so far\n",
			st.LidDestroyPendingSince.Format("15:04"), st.LidDestroyAttempts,
			humanize(time.Since(st.LidDestroyPendingSince)))
		fmt.Printf("            Last error: %s\n", st.LidDestroyReason)
		fmt.Println("            Retried on every wake while the lid stays shut; opening the lid gives up.")
	case utils.LidDestroyCancelled:
		fmt.Printf("   Lid:     🟠 a destroy owed since %s was abandoned when the lid opened — the server is still running\n",
			st.LidDestroyPendingSince.Format("15:04"))
		fmt.Printf("            %s\n", st.LidDestroyReason)
		fmt.Println("            The watcher is armed again; the next lid close destroys as usual.")
	case utils.LidDestroyOff:
		reason := st.LidDestroyReason
		if reason == "" {
			reason = "reason not recorded"
		}
		fmt.Printf("   Lid:     🔴 destroy-on-lid-close OFF (%s)\n", reason)
		fmt.Println("            The server bills until destroyed; shutting the lid will not stop it.")
	}
}

// printLeftoverLidDebt warns about a destroy that fired at a lid close and was
// never paid off.
//
// Separate from printLidDestroy because it describes a different thing: not
// what the daemon starting now will do, but what an earlier one failed to do
// and what that is still costing. Silent unless there is something to say.
//
// Reads the debt file rather than state.json, which is why it now keeps saying
// this after a `mole down && mole up`. It used to go quiet at exactly the
// moment the last thing that would have retried the destroy was killed.
func printLeftoverLidDebt() {
	debt := utils.ReadLidDebt()
	if debt == nil {
		return
	}
	who := debt.Server
	if who == "" {
		who = "the server"
	}
	fmt.Printf("⚠️  A lid close at %s failed to destroy %s (%d attempts); it has been\n",
		debt.Since.Format("2006-01-02 15:04"), who, debt.Attempts)
	fmt.Printf("   billing %s since. Destroy it with `mole down --destroy`, or keep using it —\n",
		humanize(time.Since(debt.Since)))
	fmt.Println("   Vultr bills a fresh hour on create, so replacing it now saves nothing.")
}

func humanize(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// legPinNote flags a hy2 pin on the profile line. It is worth a few characters
// because the pin holds traffic off the primary and disarms the automatic
// return: someone reading this line during a bad night needs to know mole is
// holding UDP on purpose rather than failing to notice.
func legPinNote() string {
	if utils.ReadLeg() == utils.LegHy2 {
		return " — leg pinned to hy2, REALITY primary bypassed"
	}
	return ""
}

// dialLast renders the most recent fresh-dial result for the status line: the
// time it took, or why it did not finish.
func dialLast(st *utils.State) string {
	if st.LastDialError != "" {
		return "failed: " + st.LastDialError
	}
	if st.LastDialMs > 0 {
		return fmt.Sprintf("%.2fs", float64(st.LastDialMs)/1000)
	}
	return "unknown"
}
