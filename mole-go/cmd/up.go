package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/leo/mole/config"
	"github.com/leo/mole/core"
	"github.com/leo/mole/utils"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up [config-file]",
	Short: "Start the VPN connection",
	Long:  `Start the VPN connection using the specified configuration file or default configuration.`,
	RunE:  runUp,
}

func runUp(cmd *cobra.Command, args []string) error {
	// Check if already running
	if err := utils.CheckAlreadyRunning(); err != nil {
		return err
	}

	configPath := ""
	if len(args) > 0 {
		configPath = args[0]
	}

	// Parse mole configuration
	moleConfig, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Convert to Hiddify format
	hiddifyConfig, err := config.ConvertToHiddify(moleConfig)
	if err != nil {
		return fmt.Errorf("failed to convert config: %w", err)
	}

	// Save Hiddify config to ~/.mole/
	hiddifyConfigPath := utils.HiddifyConfigPath()
	if err := config.SaveHiddifyConfig(hiddifyConfig, hiddifyConfigPath); err != nil {
		return fmt.Errorf("failed to save hiddify config: %w", err)
	}

	// Show server IP info
	if moleConfig.Server != "" {
		fmt.Println("🔍 Checking server info...")
		info, err := utils.GetIPInfo(moleConfig.Server)
		if err == nil {
			fmt.Printf("🌍 Server: %s (%s, %s)\n", info.IP, info.Country, info.City)
		}
	}

	// Write PID file
	if err := utils.WritePID(); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}
	defer utils.RemovePID()

	// Start Hiddify-core
	fmt.Println("🚀 Starting VPN connection...")
	utils.MoleLogInfo("Starting VPN connection")
	if err := core.Start(hiddifyConfigPath); err != nil {
		utils.MoleLogError(fmt.Sprintf("Failed to start VPN: %v", err))
		return fmt.Errorf("failed to start VPN: %w", err)
	}

	fmt.Println("✅ VPN connection established!")
	utils.MoleLogInfo("VPN connection established")
	fmt.Println("Press Ctrl+C to stop")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n🛑 Stopping VPN...")
	utils.MoleLogInfo("Stopping VPN")
	if err := core.Stop(); err != nil {
		utils.MoleLogError(fmt.Sprintf("Failed to stop VPN: %v", err))
		return fmt.Errorf("failed to stop VPN: %w", err)
	}

	fmt.Println("✅ VPN stopped")
	utils.MoleLogInfo("VPN stopped")
	return nil
}
