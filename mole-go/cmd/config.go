package cmd

import (
	"fmt"

	"github.com/leo/mole/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage configuration",
	Long:  `View or validate mole configuration files.`,
}

var configShowCmd = &cobra.Command{
	Use:   "show [config-file]",
	Short: "Show configuration",
	Long:  `Display the parsed configuration in a readable format.`,
	RunE:  runConfigShow,
}

var configValidateCmd = &cobra.Command{
	Use:   "validate [config-file]",
	Short: "Validate configuration",
	Long:  `Validate a configuration file for syntax and logic errors.`,
	RunE:  runConfigValidate,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configValidateCmd)
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	configPath := ""
	if len(args) > 0 {
		configPath = args[0]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	fmt.Printf("Server: %s\n", cfg.Server)
	fmt.Printf("DNS: %v\n", cfg.DNS)
	fmt.Printf("Log Level: %s\n", cfg.LogLevel)
	
	return nil
}

func runConfigValidate(cmd *cobra.Command, args []string) error {
	configPath := ""
	if len(args) > 0 {
		configPath = args[0]
	}

	_, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("❌ Configuration is invalid: %w", err)
	}

	fmt.Println("✅ Configuration is valid")
	return nil
}
