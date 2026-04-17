package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the background VPN",
	RunE:  runDown,
}

func runDown(cmd *cobra.Command, args []string) error {
	fmt.Println("🛑 Stopping VPN...")

	pid, err := utils.ReadPID()
	if err != nil || !utils.IsRunning(pid) {
		// Nothing alive — still restore DNS and scrub any lingering sing-box.
		utils.RestoreDNS()
		_ = core.Stop()
		utils.RemovePID()
		utils.RemoveState()
		fmt.Println("✅ VPN stopped")
		return nil
	}

	// The daemon runs as root; non-root users can't signal it.
	if (runtime.GOOS == "darwin" || runtime.GOOS == "linux") && os.Geteuid() != 0 {
		return reExecDownViaSudo()
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find daemon pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon: %w", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !utils.IsRunning(pid) {
			fmt.Println("✅ VPN stopped")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Escalate: kill the daemon and tear down anything it was supervising.
	fmt.Println("   daemon did not exit cleanly; forcing")
	_ = proc.Signal(syscall.SIGKILL)
	utils.RestoreDNS()
	_ = core.Stop()
	utils.RemovePID()
	utils.RemoveState()
	fmt.Println("✅ VPN stopped")
	return nil
}

func reExecDownViaSudo() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{exe}, os.Args[1:]...)
	c := exec.Command("sudo", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
