package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"

	"github.com/leo/mole/utils"
	"github.com/spf13/cobra"
)

var (
	follow bool
	tail   int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View VPN logs",
	Long:  `Display logs from the VPN connection. Use -f to follow logs in real-time.`,
	RunE:  runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow logs in real-time")
	logsCmd.Flags().IntVarP(&tail, "tail", "n", 50, "Number of lines to show from the end")
}

func runLogs(cmd *cobra.Command, args []string) error {
	logFile := utils.LogPath()

	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		fmt.Println("No logs available at " + logFile)
		return nil
	}

	if follow {
		// Follow logs like tail -f
		command := exec.Command("tail", "-f", logFile)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		return command.Run()
	}

	// Show last N lines
	file, err := os.Open(logFile)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read logs: %w", err)
	}

	// Print last N lines
	start := 0
	if len(lines) > tail {
		start = len(lines) - tail
	}

	for _, line := range lines[start:] {
		fmt.Println(line)
	}

	return nil
}
