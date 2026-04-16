package cmd

import (
	"fmt"

	"github.com/leo/mole/core"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VPN connection status",
	Long:  `Display the current status of the VPN connection.`,
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	status, err := core.GetStatus()
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status.Running {
		fmt.Println("🟢 VPN Status: Connected")
		fmt.Printf("   Server: %s\n", status.Server)
		fmt.Printf("   Uptime: %s\n", status.Uptime)
		if status.IPInfo.IP != "" {
			fmt.Printf("   Location: %s (%s)\n", status.IPInfo.Country, status.IPInfo.City)
		}
	} else {
		fmt.Println("🔴 VPN Status: Disconnected")
	}

	return nil
}
