package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

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
		// Daemon is gone — but a previous unclean exit may have left sing-box,
		// DNS, and UDP buffer tweaks behind. Reconcile from state.json.
		killOrphanedSingboxFromState()
		utils.RestoreDNS()
		utils.RestoreUDPBuffers()
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

	// Escalate: SIGKILL the daemon, then directly kill the sing-box pid the
	// daemon recorded in state.json (its own deferred Stop won't run after
	// SIGKILL). Restore DNS + UDP buffers since the daemon's defers won't
	// either.
	fmt.Println("   daemon did not exit cleanly; forcing")
	_ = proc.Signal(syscall.SIGKILL)
	for i := 0; i < 30 && utils.IsAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	killOrphanedSingboxFromState()
	utils.RestoreDNS()
	utils.RestoreUDPBuffers()
	utils.RemovePID()
	utils.RemoveState()
	fmt.Println("✅ VPN stopped")
	return nil
}

// killOrphanedSingboxFromState reads state.json for the recorded sing-box pid
// and kills it directly if still alive. Avoids the old `pkill -f sing-box`
// sledgehammer, which would also kill unrelated sing-box instances on the
// same machine.
func killOrphanedSingboxFromState() {
	st, err := utils.ReadState()
	if err != nil || st.SingboxPID == 0 {
		return
	}
	if !utils.IsAlive(st.SingboxPID) {
		return
	}
	p, err := os.FindProcess(st.SingboxPID)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	for i := 0; i < 30 && utils.IsAlive(st.SingboxPID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if utils.IsAlive(st.SingboxPID) {
		_ = p.Signal(syscall.SIGKILL)
	}
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
