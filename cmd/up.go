package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
var lidGrace time.Duration
var tuneOnStart bool

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
	upCmd.Flags().DurationVar(&lidGrace, "lid-grace", 5*time.Minute, "keep the server this long after the lid shuts; it is destroyed on the first wake after that if the lid is still shut (0 = destroy at lid close)")
	_ = upCmd.Flags().MarkDeprecated("no-destroy-on-sleep", "use --no-destroy-on-lid-close")
	// A flag rather than an environment variable on purpose: `mole up` re-execs
	// itself through sudo, and sudo's env_reset drops anything not on its
	// env_keep list — so an env-var knob would silently fail to reach the very
	// process that needs it. Arguments survive the re-exec, and Daemonize
	// forwards them again to the detached child.
	upCmd.Flags().BoolVar(&lidCloseDryRun, "lid-close-dry-run", false, "watch for a lid close and log it without destroying anything (needs no Vultr key)")
	upCmd.Flags().BoolVar(&tuneOnStart, "tune-on-start", false, "auto-scan Brutal cwnd_gain after tunnel up and pin the best value (only runs when brutal_gain is still 0, i.e. a fresh node)")
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
	// The co-hosted REALITY leg, when this node runs one. It becomes a second
	// member of the same proxy selector, so the supervisor can move traffic off
	// UDP without restarting sing-box. Empty on a single-leg node.
	standby := srv.StandbyURI()
	if standby != "" {
		fmt.Println("🎭 Standby leg: VLESS+REALITY over TCP/443 (auto-failover armed)")
	}

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

	cfg, err := config.BuildWithStandby(uri, standby)
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
	if err := core.PreflightEngine(cfgPath); err != nil {
		return fmt.Errorf("engine preflight: %w", err)
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
	// 100s, not 30s: cold-start init is serial — PF restore of the direct
	// exemptions plus the CN CIDR list, a pre-flight round-trip to the brutal
	// keeper (which waits up to 65s for a freshly deployed node to finish
	// building the optional module) — and on 2026-11-07 it took ~12s on this
	// machine, so the CLI reported "did not come up" for a daemon that was
	// healthy seconds later. A slow start is not a failed start; the daemon
	// keeps running past this timeout either way, so waiting longer only
	// removes the false alarm.
	if err := waitForDaemonReady(pid, 100*time.Second); err != nil {
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
	if err := core.PreflightEngine(cfgPath); err != nil {
		return fmt.Errorf("engine preflight before network takeover: %w", err)
	}

	// TUN needs DNS takeover on macOS. Defer restore so any clean exit path resets it.
	// Register recovery before takeover, including partially applied failures.
	defer func() {
		if err := utils.RestoreDNS(); err != nil {
			log.Printf("DNS restoration incomplete (backup retained): %v", err)
		}
	}()
	if err := utils.TakeOverDNS(); err != nil {
		return fmt.Errorf("DNS takeover failed: %w", err)
	}

	// Raise UDP socket buffers so hy2/QUIC can saturate the link on macOS, and
	// stop the interface AQM from turning Wi-Fi congestion into a killed QUIC
	// connection. See netTuning.
	if err := utils.TuneNetworkStack(); err != nil {
		fmt.Printf("⚠️  network stack tuning failed: %v\n", err)
	}
	defer utils.RestoreNetworkStack()

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
			// Replay what the direct rules were last seen to dial, and drop
			// whatever those rules no longer claim. A domain routed direct is
			// routed into the block rule above until its addresses are back in
			// the exemption table, so this runs before sing-box does.
			utils.SeedDirectExemptions(config.DirectDomains())
			defer utils.UnloadKillSwitch()
		}
	}

	armed, dryRun, offReason := lidDestroyPlan()

	// 2026-10-07: do NOT defer os.Remove(utils.VultrKeyPath()) on exit.
	// The daemon (this runDaemon process) does not own that file — it is
	// written by stashVultrKey() in runParent (up.go:142, the user-shell
	// process before sudo) and by deployViaVultr (server.go:587, the
	// `mole server deploy` CLI). Removing it here on daemon exit means
	// `mole down` wipes the key, the next `mole up` finds nothing, and
	// destroy-on-lid-close silently disarms — exactly what happened on
	// 2026-10-07 after redeploy. The key lives at 0600 under ~/.mole/,
	// the same pattern as ssh keys and kubeconfig; persistence is the
	// default and matches how users expect a long-term credential to work.

	// Probe address must use the same host mole actually connects to, which
	// IPv4 preferred but falls back to IPv6 when the node is v6-only.
	probeHost := serverHost(*srv)
	if strings.Contains(probeHost, ":") && !strings.HasPrefix(probeHost, "[") {
		probeHost = "[" + probeHost + "]"
	}
	probeAddr := fmt.Sprintf("%s:%d", probeHost, srv.Port)

	sup := core.NewSupervisor(cfgPath, srv.Name, core.SupervisorOpts{
		// Direct UDP probe to the VPS hy2 endpoint — avoids the DNS-loop bug
		// where DoT-to-1.1.1.1 hiccups got misread as VPS death and reported a
		// healthy VPS as down in `mole status`.
		ProbeAddr: probeAddr,
		// The probe has to speak the server's obfuscation or it decodes to
		// noise on arrival, goes unanswered, and reports a dark path through a
		// working tunnel.
		ObfsPassword: srv.ObfsPassword,
		// Time-of-day Brutal ceiling. Inert unless the server sets a peak profile;
		// when set, the config (built from the same server URI) carries the
		// peak/off-peak selector and the supervisor flips it by the clock.
		Schedule:        srv.Schedule(),
		RealityRateMbps: srv.RealityDownMbps,
		RealityRateAuto: srv.RealityRateAuto,
		BrutalGain:      srv.BrutalGain,
		// Arms the automatic failover to the REALITY leg. Derived from the same
		// StandbyURI the parent built the config from, so the flag and the
		// selector member it names can never disagree — selecting a member
		// sing-box never heard of would fail the call and strand traffic on a
		// leg already proven dark.
		HasRealityLeg:   srv.StandbyURI() != "",
		VerifyLegEgress: srv.StandbyURI() != "",
		// For the out-of-tunnel TCP probe that separates "the node refuses the
		// port" from "the path drops it" when the leg is being re-tested.
		RealityAddr: srv.realityAddr(),
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
		LidGrace:            lidGrace,
		LidDestroyOffReason: offReason,
		// 规则热更：守护进程监听 custom-rules.json，改完即时生效，不需要任何命令，
		// 也不重启 sing-box / 不断 TUN（见 core/ruleswatch.go 与 config/hotrules.go）。
		OnRulesChanged: RecompileHotRules,
		// 让 PF 的 direct 豁免表跟上这些规则实际拨出去的地址。没有它，
		// custom-rules.json 里任何一条指向境外的 "outbound": "direct"
		// 都会被 kill switch 自己静默丢掉（见 utils/directexempt.go）。
		DirectDomains: config.DirectDomains,
		// Resolved at call time, not closed over as srv.Name: after the first lid
		// close destroys the server, the name this daemon started with refers to
		// nothing, and asking to destroy it again used to be filed as a failed
		// destroy — a debt for a server that no longer existed.
		OnIdleDestroy: destroyActiveServerWithin,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Install the node's pacing rule BEFORE the production engine exists.
	// TCP Brutal classifies only sockets created after the rule is in place, and
	// the engine's mux pool is long-lived: a session dialled before the first
	// successful push keeps the kernel default (bbr) until it is torn down, so
	// the whole tunnel stays unpaced even though every later push succeeds.
	preflightPacing(ctx, srv)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	runErr := make(chan error, 1)
	go func() {
		runErr <- sup.Run(ctx)
	}()

	// Optional: auto-tune Brutal cwnd_gain on fresh nodes.
	// Only runs when --tune-on-start is set AND the node has no recorded gain
	// (brutal_gain == 0). Best-effort — any failure (keeper still building,
	// tunnel not ready) is silently ignored so it never blocks startup.
	if tuneOnStart && srv.BrutalGain == 0 {
		go func() {
			kurl := fmt.Sprintf("http://127.0.0.1:%d", core.BrutalKeeperPort)
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get(kurl + "/health")
				if err == nil {
					resp.Body.Close()
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			bestGain, bestMbps, _, err := scanAndPickGain(kurl)
			if err != nil {
				log.Printf("auto-tune skipped: %v", err)
				return
			}
			if err := changeServer(srv.InstanceID, srv.Name, func(s *Server) {
				s.BrutalGain = bestGain
			}); err != nil {
				log.Printf("auto-tune: write servers.json failed: %v", err)
				return
			}
			// Apply live — supervisor will re-push within 10 min anyway, but
			// applying now is what makes the new gain effective immediately.
			resp, _ := http.Get(fmt.Sprintf("%s/gain?g=%d", kurl, bestGain))
			if resp != nil {
				resp.Body.Close()
			}
			log.Printf("auto-tune: Brutal gain=%d (%.1f Mbps) written to servers.json and keeper", bestGain, bestMbps)
		}()
	}

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

// preflightPacing installs the REALITY pacing rule through a throwaway,
// non-multiplexed engine before the production engine dials. Best effort: a
// node whose optional Brutal module is still building simply keeps its old
// behaviour, and the supervisor retries the push every minute.
//
// The throwaway engine is what makes the ordering possible. The production
// forwarder is part of the config the engine starts with, so pushing through
// it would require the engine — and the engine's TUN starts taking traffic
// immediately, so some mux session could be dialled before the rule exists.
// A rate push over a non-mux socket installs the rule for this client's /32
// without ever entering the pool.
func preflightPacing(ctx context.Context, srv *Server) {
	uri, rate, gain := srv.StandbyURI(), srv.RealityDownMbps, srv.BrutalGain
	if uri == "" {
		return
	}
	if rate <= 0 {
		// A freshly deployed node has no recorded REALITY rate yet (ladder
		// memory only fills it for regions seen before), but the supervisor
		// will still pace the leg — at the schedule's current ceiling rung
		// (core/supervisor.go: tcpRate = down when realityRateMbps == 0).
		// Install the rule at that same rate before the engine dials; the
		// first runtime push re-rates the existing rule seconds later.
		// Skipping here is how the nrt-trial deploy on 2026-11-09 went back
		// to the old race: pool dialled unpaced.
		_, _, rate = srv.Schedule().Profile(time.Now())
		if rate <= 0 {
			return
		}
	}
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		return
	}
	out, err := config.ParseOutbound(uri)
	if err != nil {
		log.Printf("brutal preflight: standby identity unusable: %v", err)
		return
	}
	out.Tag = "rate-leg"
	out.Multiplex = nil

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("brutal preflight: no local port: %v", err)
		return
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	dir, err := os.MkdirTemp("", "mole-rate-")
	if err != nil {
		log.Printf("brutal preflight: %v", err)
		return
	}
	defer os.RemoveAll(dir)
	cfg := map[string]any{
		"log": map[string]any{"level": "error"},
		"inbounds": []any{map[string]any{
			"type": "direct", "tag": "rate", "listen": "127.0.0.1", "listen_port": port,
			"override_address": "127.0.0.1", "override_port": core.BrutalKeeperPort,
		}},
		"outbounds": []any{out, map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"rules": []any{map[string]any{"inbound": []string{"rate"}, "action": "route", "outbound": out.Tag}},
			"final": "direct",
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("brutal preflight: %v", err)
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "engine.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("brutal preflight: %v", err)
		return
	}
	defer logFile.Close()
	cmd := exec.CommandContext(ctx, bin, "run", "-c", path)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		log.Printf("brutal preflight: engine did not start: %v", err)
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	select {
	case <-ctx.Done():
		return
	case <-time.After(250 * time.Millisecond):
	}

	// A freshly deployed node installs the optional module asynchronously and
	// can take a minute to build it; waiting here is what stops that minute
	// from pinning the entire session to bbr.
	deadline := time.Now().Add(65 * time.Second)
	warned := false
	for {
		res, err := core.PushBrutalRateViaWithGain(port, rate, gain, 5000)
		if !warned && (err != nil || res.Applied == 0) {
			detail := "keeper did not confirm the rule"
			if err != nil {
				detail = err.Error()
			}
			log.Printf("brutal preflight: %s; waiting up to 65s for the optional module", detail)
			warned = true
		}
		if err == nil && res.Applied > 0 {
			// The first add lands the rule and its route, but keeper's loop
			// (apply() → sleep 3s) may not have re-routed an existing peer
			// yet. Poll the keeper every 200ms with a re-push: as soon as it
			// reports Applied>0 the rule is re-locked in, no need to wait the
			// full 3s. Worst case falls back to a hard 3s — same ceiling as
			// before, but a healthy keeper can confirm in 500–800ms.
			settle := time.Now().Add(3 * time.Second)
			confirmed := false
			for time.Now().Before(settle) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
				r2, err2 := core.PushBrutalRateViaWithGain(port, rate, gain, 5000)
				if err2 == nil && r2.Applied > 0 {
					res.Peers = r2.Peers
					res.Applied = r2.Applied
					confirmed = true
					break
				}
			}
			// If the keeper never confirmed a second apply, we still continue —
			// the first push did land; this is just "please confirm".
			_ = confirmed
			log.Printf("brutal: rule installed before the engine dials (↓%d Mbps, %d peer(s))", rate, res.Peers)
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				log.Printf("brutal preflight: no keeper answer (%v); REALITY may start unpaced", err)
			} else {
				log.Printf("brutal preflight: rate not applied (%s); REALITY may start unpaced", res.Detail)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
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

	if err := changeServer(target.InstanceID, target.Name, func(s *Server) { s.Stopped = false; s.Verification = nil }); err != nil {
		return err
	}
	fmt.Printf("✅ %s started\n", target.Name)
	return nil
}
