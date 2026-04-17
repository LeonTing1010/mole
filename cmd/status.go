package cmd

import (
	"fmt"

	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VPN status and current exit IP",
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
