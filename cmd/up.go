package cmd

import (
	"context"
	"encoding/json"
	"errors"
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
var noKillSwitch bool
var noDestroyOnLidClose bool
var lidCloseDryRun bool

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
	upCmd.Flags().BoolVar(&noKillSwitch, "no-killswitch", false, "skip the PF kill switch (foreign traffic may leak to the physical interface if sing-box dies)")
	upCmd.Flags().BoolVar(&noDestroyOnLidClose, "no-destroy-on-lid-close", false, "keep the server when the lid closes (it bills until destroyed, not until stopped)")
	// The old spelling, kept working because it is in people's shell history.
	// It was never accurate about what fired: idle and maintenance sleep counted
	// too, which is the bug this rename came out of.
	upCmd.Flags().BoolVar(&noDestroyOnLidClose, "no-destroy-on-sleep", false, "")
	_ = upCmd.Flags().MarkDeprecated("no-destroy-on-sleep", "use --no-destroy-on-lid-close")
	// A flag rather than an environment variable on purpose: `mole up` re-execs
	// itself through sudo, and sudo's env_reset drops anything not on its
	// env_keep list — so an env-var knob would silently fail to reach the very
	// process that needs it. Arguments survive the re-exec, and Daemonize
	// forwards them again to the detached child.
	upCmd.Flags().BoolVar(&lidCloseDryRun, "lid-close-dry-run", false, "watch for a lid close and log it without destroying anything (needs no Vultr key)")
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

	// `sudo mole up` can never arm destroy-on-lid-close, so refuse it instead of
	// warning about it. sudo runs the binary rather than the shell, so a
	// wrapper that loads the key on demand never fires, and env_reset drops
	// VULTR_API_KEY even when one was exported — the stash below is guaranteed
	// to fail. This exact invocation left a server billing for 22 hours behind
	// a warning nobody was still looking at. There is no reason to reach for
	// sudo here: `mole up` re-execs itself through sudo one step further down,
	// after the key has been copied to where the daemon can read it.
	if sudoWouldDisarmLidDestroy(os.Geteuid(), os.Getenv("SUDO_USER"), vultrKeyAvailable(), noDestroyOnLidClose, lidCloseDryRun) {
		return fmt.Errorf("run `mole up` WITHOUT sudo — it elevates itself.\n" +
			"    Under sudo the Vultr key cannot be read (env_reset), so the server\n" +
			"    would keep billing after the lid is shut. To bring the tunnel up\n" +
			"    anyway, pass --no-destroy-on-lid-close and destroy it by hand later")
	}

	// Auto-wake BEFORE sudo: the wake only needs VULTR_API_KEY (not root),
	// and sudo strips environment variables, so we must do this while the
	// user's env is still intact.
	// Both recoveries run before sudo for the same reason, and both are
	// deterministic: there is exactly one correct action and no judgement to
	// make, so asking the operator to perform it by hand only adds a step that
	// can be forgotten. Anything genuinely ambiguous still stops and says so.
	if _, err := ActiveServer(); err != nil {
		switch {
		case isStoppedError(err):
			fmt.Println("⏸️  Active server is stopped, waking it up...")
			if err := autoWakeServer(); err != nil {
				return fmt.Errorf("auto-wake failed: %w", err)
			}
		case isProvisioningError(err):
			// An entry recorded mid-deploy is missing only its address, and
			// only Vultr can supply it. Fetch it rather than reporting the
			// server unusable and naming a command for the operator to run.
			fmt.Println("⏳ Active server has no address yet, fetching it...")
			if _, rerr := reconcileAddresses(); rerr != nil {
				return fmt.Errorf("could not record the server's address: %w", rerr)
			}
			if _, rerr := ActiveServer(); rerr != nil {
				return fmt.Errorf("%w (Vultr has not assigned an address yet — retry shortly)", rerr)
			}
		}
	}

	// Destroy-on-lid-close fires from inside the daemon, which sudo will have
	// stripped the environment from, so the key has to be stashed here — the
	// same pre-sudo window, and for the same reason, as the recoveries above.
	//
	// A missing key disarms the feature rather than failing the command: `mole
	// up` has never needed one, and refusing to bring up a tunnel over a
	// billing optimisation would be the wrong trade. Say so, though — silently
	// not destroying is exactly the surprise that shows up on an invoice.
	//
	// A failed stash is only fatal to the feature when it leaves no usable key
	// at all: this function runs a second time inside the sudo re-exec, where
	// the environment is gone by definition but the file the first pass wrote is
	// right there. Warning on that pass would print the alarm on every single
	// healthy startup, which is the fastest way to teach someone to ignore it.
	if !noDestroyOnLidClose && !lidCloseDryRun {
		if err := stashVultrKey(); err != nil && !vultrKeyAvailable() {
			fmt.Printf("⚠️  destroy-on-lid-close is OFF: %v\n", err)
			fmt.Println("    The server will keep billing after this Mac is shut.")
			noDestroyOnLidClose = true
		}
	}

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

	// Pre-cache geo rule-sets locally so sing-box can start even if the VPS
	// is unreachable. Without this, a dead VPS becomes a hard startup failure
	// (rule-sets routed through the proxy outbound) instead of a soft
	// supervisor-detected outage.
	utils.EnsureRuleSets()

	// Pre-cache the curated direct-domain list (oray/qq.com/…) from the source
	// repo, so it can update without recompiling. Falls back to the embedded
	// copy if the fetch fails; never blocks startup.
	utils.EnsureBuiltinRules()

	// Pre-cache the China IPv4 CIDR list the kill-switch PF table loads from.
	// Same best-effort contract; the daemon falls back to the embedded copy.
	if !noKillSwitch {
		utils.EnsureCNCIDRs()
	}

	cfg, err := config.Build(uri)
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}

	// Validate config doesn't reference missing rule-sets
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	cfgPath := utils.SingboxConfigPath()
	if err := config.Save(cfg, cfgPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("📋 Config: %s\n", cfgPath)

	fmt.Println("🚀 Starting VPN...")

	// Rotate previous log to a timestamped backup so a failed run's evidence
	// survives the next `mole up` retry. Keep the newest 5; older ones are
	// rarely useful and just consume disk.
	_ = utils.RotateLog(5)

	// A destroy that fired and never completed outlives the daemon that wrote
	// it — the server it failed to delete is still running — so say so on the
	// way up too. No longer the last moment anyone is told: the record survives
	// this restart now, and `mole status` will keep showing it.
	printLeftoverLidDebt()

	pid, err := utils.Daemonize("--internal-daemon")
	if err != nil {
		return fmt.Errorf("daemonize: %w", err)
	}

	// Wait for the daemon to write its state (indicates sing-box came up).
	if err := waitForDaemonReady(pid, 10*time.Second); err != nil {
		return fmt.Errorf("daemon did not come up: %w (see %s)", err, utils.LogPath())
	}

	fmt.Printf("✅ Running in background (pid %d)\n", pid)
	// Read it back from the daemon rather than reprinting what this process
	// intended: the daemon is what actually re-checked the key, and it is the
	// same line `mole status` will show, so there is one answer and not two.
	if st, err := utils.ReadState(); err == nil {
		printLidDestroy(st)
	}
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

	// Arm the PF kill switch BEFORE sing-box starts: foreign egress is only
	// allowed through the tunnel, so there is no window — not even during
	// startup — where a foreign connection can leak out of the physical
	// interface. Domestic (CN) traffic stays direct and unrestricted.
	// Deliberately NOT unloaded on daemon death by any supervisor: if this
	// process is SIGKILL'd the anchor stays armed and foreign stays blocked
	// (fail-closed) until the next `mole up`/`mole down`.
	if runtime.GOOS == "darwin" && !noKillSwitch {
		// Every recorded server, not just the active one: a probe to a standby
		// node also leaves a physical interface, and blocking it would report
		// that node dark whether or not it is.
		if err := utils.LoadKillSwitch(knownServerIPs()...); err != nil {
			fmt.Printf("⚠️  kill switch failed to arm: %v — foreign traffic will LEAK direct if sing-box dies\n", err)
		} else {
			fmt.Println("🛡️  kill switch armed: foreign egress locked to the tunnel, CN direct unrestricted")
			defer utils.UnloadKillSwitch()
		}
	}

	armed, dryRun, offReason := lidDestroyPlan()

	// The stashed key is only needed while this daemon runs. Remove it on any
	// clean exit so the extra copy of a root-scoped credential does not outlive
	// the feature that asked for it.
	if armed && !dryRun {
		defer os.Remove(utils.VultrKeyPath())
	}

	core.SetServerAddress(srv.IP)

	sup := core.NewSupervisor(cfgPath, srv.Name, core.SupervisorOpts{
		// Direct UDP probe to the VPS hy2 endpoint — avoids the DNS-loop bug
		// where DoT-to-1.1.1.1 hiccups got misread as VPS death and reported a
		// healthy VPS as down in `mole status`.
		ProbeAddr: fmt.Sprintf("%s:%d", srv.IP, srv.Port),
		// The probe has to speak the server's obfuscation or it decodes to
		// noise on arrival, goes unanswered, and reports a dark path through a
		// working tunnel.
		ObfsPassword: srv.ObfsPassword,
		// Time-of-day Brutal ceiling. Inert unless the server sets a peak profile;
		// when set, the config (built from the same server URI) carries the
		// peak/off-peak selector and the supervisor flips it by the clock.
		Schedule: srv.Schedule(),
		// Idle-destroy. Off unless asked for; when on, the watcher deletes this
		// server once nothing has gone abroad for the configured window and then
		// stops the daemon, so the deferred DNS restore and kill-switch unload
		// above run and the machine is left in a normal state rather than one
		// where every foreign connection is fail-closed against a server that no
		// longer exists.
		// On by default. Closing the lid is the one unambiguous "done for now" a
		// laptop produces, and the only such signal that survives the host going
		// to sleep — anything measured by a timer or by traffic simply freezes
		// there. lidDestroyPlan re-checks the key rather than trusting the parent
		// stashed it: this process may have been started straight from the
		// daemon path.
		DestroyOnLidClose:   armed,
		LidCloseDryRun:      dryRun,
		LidDestroyOffReason: offReason,
		// 规则热更：守护进程监听 custom-rules.json，改完即时生效，不需要任何命令，
		// 也不重启 sing-box / 不断 TUN（见 core/ruleswatch.go 与 config/hotrules.go）。
		OnRulesChanged: RecompileHotRules,
		// Resolved at call time, not closed over as srv.Name: after the first lid
		// close destroys the server, the name this daemon started with refers to
		// nothing, and asking to destroy it again used to be filed as a failed
		// destroy — a debt for a server that no longer existed.
		OnIdleDestroy: destroyActiveServerWithin,
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

// lidDestroyPlan decides what the daemon will do when the lid is shut, and says
// why in the operator's terms when the answer is "nothing".
//
// The reason is the point. Every branch here was reachable before and produced
// the same observable behaviour — a daemon that quietly never destroys anything
// — so the only way to tell an intentional opt-out from a key that failed to
// arrive was to already suspect the second one.
func lidDestroyPlan() (armed, dryRun bool, offReason string) {
	switch {
	case noDestroyOnLidClose:
		return false, false, "--no-destroy-on-lid-close was passed"
	case lidCloseDryRun:
		return true, true, ""
	case !vultrKeyAvailable():
		return false, false, "no Vultr key reached the daemon — `mole up` ran without VULTR_API_KEY in its environment (running it under sudo does this)"
	}
	return true, false, ""
}

// sudoWouldDisarmLidDestroy reports whether this process is an operator's `sudo
// mole up` that is certain to end up with the watcher disarmed — as opposed to
// the sudo re-exec `mole up` performs on itself, which arrives here with the
// key already stashed and is the normal path.
func sudoWouldDisarmLidDestroy(euid int, sudoUser string, keyAvailable, optedOut, dryRun bool) bool {
	if optedOut || dryRun || keyAvailable {
		return false
	}
	return euid == 0 && sudoUser != ""
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

// isStoppedError reports whether err is the "server is stopped" error from ActiveServer.
func isStoppedError(err error) bool { return errors.Is(err, ErrServerStopped) }

// isProvisioningError reports the one failure that fixes itself: a local entry
// created during a deploy whose address had not appeared yet.
func isProvisioningError(err error) bool { return errors.Is(err, ErrServerProvisioning) }

// autoWakeServer starts the stopped active server and waits for it to be ready.
func autoWakeServer() error {
	servers, err := loadServers()
	if err != nil {
		return err
	}
	var idx int = -1
	for i, s := range servers {
		if s.Active {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no active server")
	}
	target := servers[idx]

	if target.InstanceID == "" {
		return fmt.Errorf("server has no instance ID")
	}

	_, err = serverAPI("POST", "/instances/"+target.InstanceID+"/start", nil)
	if err != nil {
		return fmt.Errorf("start instance: %w", err)
	}

	// Wait for the instance to actually come up, and say so if it doesn't.
	// This loop used to fall out after 60s and clear Stopped regardless, which
	// recorded the server as awake on the strength of having asked — `mole up`
	// then built a tunnel to a halted box and left the supervisor to discover
	// it. Automatic recovery is worth having precisely because it is verified;
	// an unverified one just moves the failure somewhere less obvious.
	fmt.Println("   Waiting for instance to be ready...")
	deadline := time.Now().Add(3 * time.Minute)
	running := false
	for !running && time.Now().Before(deadline) {
		resp, err := serverAPI("GET", "/instances/"+target.InstanceID, nil)
		if err == nil {
			var r struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(resp, &r) == nil && r.Status == "active" {
				running = true
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if !running {
		return fmt.Errorf("start was accepted but %s is still not running after 3m", target.Name)
	}

	servers[idx].Stopped = false
	if err := saveServers(servers); err != nil {
		return err
	}
	fmt.Printf("✅ %s started\n", target.Name)
	return nil
}
