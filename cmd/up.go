package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the VPN with the active server",
	RunE:  runUp,
}

func runUp(_ *cobra.Command, _ []string) error {
	if err := utils.CheckAlreadyRunning(); err != nil {
		return err
	}

	srv, err := ActiveServer()
	if err != nil {
		return err
	}

	uri := srv.URI()
	cfg, err := config.Build(uri)
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}

	cfgPath := utils.SingboxConfigPath()
	if err := config.Save(cfg, cfgPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if info, err := utils.GetIPInfo(srv.IP); err == nil {
		fmt.Printf("🌍 %s — %s, %s\n", srv.Name, info.Country, info.City)
	}

	if err := utils.WritePID(); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer utils.RemovePID()

	fmt.Println("🚀 Starting VPN...")
	if err := core.Start(cfgPath); err != nil {
		return fmt.Errorf("start vpn: %w", err)
	}
	core.SetServerAddress(srv.IP)
	fmt.Println("✅ Connected. Ctrl+C to stop.")
	fmt.Printf("📄 Logs: %s\n", utils.LogPath())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n🛑 Stopping...")
	if err := core.Stop(); err != nil {
		return fmt.Errorf("stop vpn: %w", err)
	}
	fmt.Println("✅ Stopped.")
	return nil
}
