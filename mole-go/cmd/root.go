package cmd

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "mole",
	Short: "Deploy a VPS, run a VPN — that simple.",
}

func Execute() error { return rootCmd.Execute() }

func init() {
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(useCmd)
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(statusCmd)
}
