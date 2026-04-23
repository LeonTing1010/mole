package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var internalDaemon bool

// maskURI replaces the user:password portion of a URI with "***" for logging.
func maskURI(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("***")
	return u.String()
}

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the VPN with the active server (runs in background)",
	RunE:  runUp,
}

func init() {
	upCmd.Flags().BoolVar(&internalDaemon, "internal-daemon", false, "")
	_ = upCmd.Flags().MarkHidden("internal-daemon")
}

func runUp(_ *cobra.Command, _ []string) error {
	if internalDaemon {
		return runDaemon()
	}
	return runParent()
}

// runParent prepares config in the foreground, daemonizes the daemon child,
// and prints user-facing status before exiting.
func runParent() error {
	if err := utils.CheckAlreadyRunning(); err != nil {
		return err
	}
	// CheckAlreadyRunning passed → no live mole daemon. But a previous daemon
	// may have been SIGKILL'd / OOM'd, leaving its sing-box child orphaned and
	// still holding TUN + the Clash API port. Sweep both state.json and
	// ps(1) to clean any leftover sing-box before we daemonize.
	core.KillOrphanedSingboxes(utils.SingboxConfigPath())

	// TUN needs root. Re-exec through sudo so the daemon child inherits root
	// and can launch sing-box directly (no sudo prompt in the detached child).
	if (runtime.GOOS == "darwin" || runtime.GOOS == "linux") && os.Geteuid() != 0 {
		return reExecViaSudo()
	}

	srv, err := ActiveServer()
	if err != nil {
		return err
	}

	uri := srv.URI()
	fmt.Printf("🔗 URI: %s\n", maskURI(uri))

	cfg, err := config.Build(uri)
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}

	cfgPath := utils.SingboxConfigPath()
	if err := config.Save(cfg, cfgPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("📋 Config: %s\n", cfgPath)

	if info, err := utils.GetIPInfo(srv.IP); err == nil {
		fmt.Printf("🌍 %s — %s, %s\n", srv.Name, info.Country, info.City)
	}

	fmt.Println("🚀 Starting VPN...")

	// Truncate previous log so the user sees a fresh run.
	if f, err := os.OpenFile(utils.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
		f.Close()
	}

	pid, err := utils.Daemonize("--internal-daemon")
	if err != nil {
		return fmt.Errorf("daemonize: %w", err)
	}

	// Wait for the daemon to write its state (indicates sing-box came up).
	if err := waitForDaemonReady(pid, 10*time.Second); err != nil {
		return fmt.Errorf("daemon did not come up: %w (see %s)", err, utils.LogPath())
	}

	fmt.Printf("✅ Running in background (pid %d)\n", pid)
	fmt.Printf("📄 Logs:   %s\n", utils.LogPath())
	fmt.Printf("📊 Status: mole status\n")
	fmt.Printf("🛑 Stop:   mole down\n")
	return nil
}

// runDaemon is the detached child: it takes over DNS, starts sing-box, and
// runs the supervisor until SIGTERM/SIGINT.
func runDaemon() error {
	if err := utils.WritePID(); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer utils.RemovePID()
	defer utils.RemoveState()

	srv, err := ActiveServer()
	if err != nil {
		return err
	}

	cfgPath := utils.SingboxConfigPath()

	// TUN needs DNS takeover on macOS. Defer restore so any clean exit path resets it.
	if err := utils.TakeOverDNS(); err != nil {
		fmt.Printf("⚠️  DNS takeover failed: %v\n", err)
	}
	defer utils.RestoreDNS()

	// Raise UDP socket buffers so hy2/QUIC can saturate the link on macOS.
	if err := utils.TuneUDPBuffers(); err != nil {
		fmt.Printf("⚠️  UDP buffer tuning failed: %v\n", err)
	}
	defer utils.RestoreUDPBuffers()

	core.SetServerAddress(srv.IP)

	sup := core.NewSupervisor(cfgPath, srv.Name, core.SupervisorOpts{
		// Direct UDP probe to the VPS hy2 endpoint — avoids the DNS-loop bug
		// where DoT-to-1.1.1.1 hiccups got misread as VPS death and chopped
		// the user's traffic into pieces.
		ProbeAddr: fmt.Sprintf("%s:%d", srv.IP, srv.Port),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	runErr := make(chan error, 1)
	go func() {
		runErr <- sup.Run(ctx)
	}()

	select {
	case sig := <-sigChan:
		fmt.Printf("received %s, stopping\n", sig)
	case err := <-runErr:
		if err != nil {
			return err
		}
		return nil
	}

	sup.Stop()
	cancel()
	select {
	case <-sup.Done():
	case <-time.After(10 * time.Second):
	}
	return nil
}

func reExecViaSudo() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	args := append([]string{exe}, os.Args[1:]...)
	c := exec.Command("sudo", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func waitForDaemonReady(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		// Check state file first (indicates sing-box started successfully)
		if st, err := utils.ReadState(); err == nil && st.PID == pid {
			return nil
		}

		// Check if PID file exists with our PID
		pidFromFile, err := utils.ReadPID()
		if err == nil && pidFromFile != pid {
			// PID file exists but has different PID - another instance is running
			return fmt.Errorf("daemon exited early")
		}
		// If PID file doesn't exist yet (err != nil) or matches our PID,
		// continue waiting for the state file

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
