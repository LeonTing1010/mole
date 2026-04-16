package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage VPS servers",
	Long:  `Deploy, destroy, and manage VPS servers for VPN.`,
}

var serverDeployCmd = &cobra.Command{
	Use:   "deploy [region] [plan] [name]",
	Short: "Deploy a new VPS",
	Long:  `Deploy a new VPS server with specified region, plan, name, port and protocol.`,
	RunE:  runServerDeploy,
}

var serverDestroyCmd = &cobra.Command{
	Use:   "destroy [name]",
	Short: "Destroy a VPS",
	Long:  `Destroy a VPS server by name.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runServerDestroy,
}

var serverLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list", "status"},
	Short:   "List all servers",
	Long:    `List all deployed VPS servers and their status.`,
	RunE:    runServerLs,
}

func init() {
	rootCmd.AddCommand(serverCmd)
	serverCmd.AddCommand(serverDeployCmd)
	serverCmd.AddCommand(serverDestroyCmd)
	serverCmd.AddCommand(serverLsCmd)

	// Flags for deploy
	serverDeployCmd.Flags().String("region", "nrt", "Server region (e.g., nrt, sgp, lax)")
	serverDeployCmd.Flags().String("plan", "vc2-1c-1gb", "Server plan (e.g., vc2-1c-1gb)")
	serverDeployCmd.Flags().String("name", "", "Server name (auto-generated if empty)")
	serverDeployCmd.Flags().Int("port", 0, "Custom port (auto-generated if 0)")
	serverDeployCmd.Flags().String("protocol", "hysteria2", "Protocol (hysteria2, vless, vmess, trojan)")
}

func runServerDeploy(cmd *cobra.Command, args []string) error {
	region, _ := cmd.Flags().GetString("region")
	plan, _ := cmd.Flags().GetString("plan")
	name, _ := cmd.Flags().GetString("name")
	port, _ := cmd.Flags().GetInt("port")
	protocol, _ := cmd.Flags().GetString("protocol")

	// If positional args provided, use them
	if len(args) > 0 {
		region = args[0]
	}
	if len(args) > 1 {
		plan = args[1]
	}
	if len(args) > 2 {
		name = args[2]
	}

	fmt.Printf("🚀 Deploying server...\n")
	fmt.Printf("   Region: %s\n", region)
	fmt.Printf("   Plan: %s\n", plan)
	fmt.Printf("   Protocol: %s\n", protocol)
	if port > 0 {
		fmt.Printf("   Port: %d\n", port)
	}
	if name != "" {
		fmt.Printf("   Name: %s\n", name)
	}

	// Check if deploy script exists
	deployScript := os.ExpandEnv("$HOME/.mole/deploy.sh")
	if _, err := os.Stat(deployScript); os.IsNotExist(err) {
		// Try to use built-in deploy logic or call Rust mole if available
		rustMole := os.ExpandEnv("$HOME/.cargo/bin/mole")
		if _, err := os.Stat(rustMole); err == nil {
			// Call Rust mole for server deploy
			rustArgs := []string{"server", "deploy", "--region", region, "--plan", plan}
			if name != "" {
				rustArgs = append(rustArgs, "--name", name)
			}
			if port > 0 {
				rustArgs = append(rustArgs, "--port", fmt.Sprintf("%d", port))
			}
			if protocol != "" {
				rustArgs = append(rustArgs, "--protocol", protocol)
			}
			execCmd := exec.Command(rustMole, rustArgs...)
			execCmd.Stdout = os.Stdout
			execCmd.Stderr = os.Stderr
			return execCmd.Run()
		}
		return fmt.Errorf("deploy script not found: %s", deployScript)
	}

	// Call deploy script with all arguments
	scriptArgs := []string{region, plan}
	if name != "" {
		scriptArgs = append(scriptArgs, name)
	}
	if port > 0 {
		scriptArgs = append(scriptArgs, fmt.Sprintf("%d", port))
	}
	if protocol != "" {
		scriptArgs = append(scriptArgs, protocol)
	}
	execCmd := exec.Command(deployScript, scriptArgs...)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	return execCmd.Run()
}

func runServerDestroy(cmd *cobra.Command, args []string) error {
	name := args[0]

	fmt.Printf("🗑️  Destroying server: %s\n", name)

	// Check if destroy script exists or call Rust mole
	rustMole := os.ExpandEnv("$HOME/.cargo/bin/mole")
	if _, err := os.Stat(rustMole); err == nil {
		execCmd := exec.Command(rustMole, "server", "destroy", name)
		execCmd.Stdout = os.Stdout
		execCmd.Stderr = os.Stderr
		return execCmd.Run()
	}

	return fmt.Errorf("server destroy not implemented in Go version yet")
}

func runServerLs(cmd *cobra.Command, args []string) error {
	fmt.Println("📋 Server List:")

	// Check if Rust mole exists and has servers
	rustMole := os.ExpandEnv("$HOME/.cargo/bin/mole")
	if _, err := os.Stat(rustMole); err == nil {
		// Try to get server list from Rust mole
		output, err := exec.Command(rustMole, "server", "ls").Output()
		if err == nil && len(output) > 0 {
			fmt.Print(string(output))
			return nil
		}
	}

	// Read from servers.json if exists
	serversFile := os.ExpandEnv("$HOME/.mole/servers.json")
	if data, err := os.ReadFile(serversFile); err == nil && len(data) > 2 {
		var servers []map[string]interface{}
		if err := json.Unmarshal(data, &servers); err == nil {
			for _, server := range servers {
				fmt.Printf("  - %s (%s:%v) [%s]\n",
					server["name"],
					server["ip"],
					server["port"],
					server["protocol"])
			}
		} else {
			fmt.Println(string(data))
		}
	} else {
		fmt.Println("No servers deployed yet.")
		fmt.Println("Use 'mole server deploy' to deploy a new server.")
	}

	return nil
}
