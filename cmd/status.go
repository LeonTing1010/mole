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
	// The supervisor only reports health; it never reroutes. Surface a probe
	// failure inline so a dead VPS is visible, but be honest that traffic is
	// still pointed at the proxy (foreign connections will time out, not
	// fail-fast).
	switch st.Mode {
	case utils.ModeProxy:
		switch {
		case st.LastProbeError != "":
			fmt.Printf("   Mode:    🔴 proxy — VPS probe failing: %s\n", st.LastProbeError)
		case st.LastLatencyMs > 0:
			fmt.Printf("   Mode:    🟢 proxy — VPS healthy (%dms)\n", st.LastLatencyMs)
		default:
			fmt.Println("   Mode:    🟢 proxy — VPS healthy")
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
	// In-tunnel keepalive — informational. A failure here does not mean the VPS
	// is down (that's the probe's verdict above); it means the proxy path itself
	// (DNS + QUIC) hiccupped on the last beat.
	if !st.LastKeepaliveAt.IsZero() {
		if st.LastKeepaliveError != "" {
			fmt.Printf("   Tunnel:  ⚠️  keepalive failed %s ago: %s\n",
				humanize(time.Since(st.LastKeepaliveAt)), st.LastKeepaliveError)
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
