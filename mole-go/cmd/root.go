package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "mole",
	Short: "A simple VPN client based on Hiddify-core",
	Long: `mole is a command-line VPN client that wraps Hiddify-core
for stable TUN mode support and easy configuration.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(configCmd)
}
