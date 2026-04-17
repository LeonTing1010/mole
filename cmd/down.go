package cmd

import (
	"fmt"

	"github.com/LeonTing1010/mole/core"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the VPN connection",
	Long:  `Stop the active VPN connection if one is running.`,
	RunE:  runDown,
}

func runDown(cmd *cobra.Command, args []string) error {
	fmt.Println("🛑 Stopping VPN...")
	
	if err := core.Stop(); err != nil {
		return fmt.Errorf("failed to stop VPN: %w", err)
	}
	
	fmt.Println("✅ VPN stopped")
	return nil
}
