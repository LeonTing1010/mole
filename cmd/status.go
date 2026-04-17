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
	switch st.Mode {
	case utils.ModeProxy:
		if st.LastLatencyMs > 0 {
			fmt.Printf("   Mode:    🟢 proxy — VPS healthy (%dms)\n", st.LastLatencyMs)
		} else {
			fmt.Println("   Mode:    🟢 proxy — VPS healthy")
		}
	case utils.ModeBlock:
		reason := st.LastProbeError
		if reason == "" {
			reason = "VPS unreachable"
		}
		fmt.Printf("   Mode:    🔴 block — %s\n", reason)
	default:
		fmt.Printf("   Mode:    %s\n", st.Mode)
	}
	if !st.LastProbeAt.IsZero() {
		fmt.Printf("   Probed:  %s ago\n", humanize(time.Since(st.LastProbeAt)))
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
