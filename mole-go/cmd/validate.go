package cmd

import (
	"fmt"

	"github.com/leo/mole/config"
	"github.com/leo/mole/utils"
	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate [config-file]",
	Short: "Validate configuration file",
	Long:  `Validate a mole configuration file for syntax and logic errors.`,
	RunE:  runValidate,
}

func init() {
	rootCmd.AddCommand(validateCmd)
}

func runValidate(cmd *cobra.Command, args []string) error {
	configPath := ""
	if len(args) > 0 {
		configPath = args[0]
	}

	// Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("❌ Configuration is invalid: %w", err)
	}

	fmt.Println("✅ Configuration is valid")
	fmt.Printf("   Server: %s\n", cfg.Server)
	fmt.Printf("   DNS: %v\n", cfg.DNS)
	fmt.Printf("   Log Level: %s\n", cfg.LogLevel)
	fmt.Printf("   TUN Enabled: %v\n", cfg.TUN.Enabled)
	fmt.Printf("   TUN MTU: %d\n", cfg.TUN.MTU)

	// Convert to Hiddify config
	hiddifyCfg, err := config.ConvertToHiddify(cfg)
	if err != nil {
		return fmt.Errorf("❌ Failed to convert to Hiddify config: %w", err)
	}

	fmt.Println("\n✅ Hiddify config conversion successful")
	fmt.Printf("   Inbounds: %d\n", len(hiddifyCfg.Inbounds))
	fmt.Printf("   Outbounds: %d\n", len(hiddifyCfg.Outbounds))
	fmt.Printf("   Route Rules: %d\n", len(hiddifyCfg.Route.Rules))

	// Save Hiddify config
	hiddifyConfigPath := utils.HiddifyConfigPath()
	if err := config.SaveHiddifyConfig(hiddifyCfg, hiddifyConfigPath); err != nil {
		return fmt.Errorf("❌ Failed to save Hiddify config: %w", err)
	}

	fmt.Printf("\n✅ Hiddify config saved to: %s\n", hiddifyConfigPath)

	return nil
}
