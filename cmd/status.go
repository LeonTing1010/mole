package cmd

import (
	"fmt"
	"time"

	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VPN status, current mode, and exit IP",
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
		line := fmt.Sprintf("   Active:  %s (%s)", active.Name, active.IP)
		if info, err := utils.GetIPInfo(active.IP); err == nil && info.Country != "" {
			line = fmt.Sprintf("   Active:  %s — %s, %s (%s)", active.Name, info.Country, info.City, active.IP)
		}
		fmt.Println(line)
	} else {
		fmt.Println("   Active:  none — run `mole server deploy`")
	}

	if running {
		printSupervisorState()
		if info, err := utils.GetMyIPInfo(); err == nil {
			fmt.Printf("   Exit IP: %s — %s, %s\n", info.IP, info.Country, info.City)
		} else {
			fmt.Printf("   Exit IP: (lookup failed: %v)\n", err)
		}
	} else if active != nil {
		fmt.Println("   Run `mole up` to connect.")
	}
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
		case utils.HealthPathDead:
			// Two flavors, same advice: restarting won't help, it usually
			// clears on its own in ~10–15min.
			if st.ConsecutiveFails >= utils.ProbeSilentThreshold {
				fmt.Printf("   Mode:    🟡 proxy — UDP path to VPS dark (probe %s ×%d); wait it out or `mole down`, restarting won't help\n",
					st.LastProbeVerdict, st.ConsecutiveFails)
			} else {
				fmt.Printf("   Mode:    🟡 proxy — VPS reachable but tunnel dark for %s (keepalive ×%d); wait it out or `mole down`, restarting won't help\n",
					humanize(time.Since(st.KeepaliveFailingSince)), st.KeepaliveFails)
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
	if st.BandwidthProfile != "" {
		fmt.Printf("   Profile: %s (Brutal ↓%d Mbps)\n", st.BandwidthProfile, st.BandwidthDownMbps)
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
