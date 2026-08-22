package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var destroyOnDown bool

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the background VPN",
	RunE:  runDown,
}

func init() {
	downCmd.Flags().BoolVar(&destroyOnDown, "destroy", false, "also destroy the active server (it bills until destroyed, not until stopped)")
}

// destroyActiveServerForDown deletes the active server on the way down.
//
// This is the complement to `mole up --idle-destroy`, and it covers the case
// that one structurally cannot: the watcher lives inside the daemon, so it can
// only notice disuse while mole is running. Going to bed with `mole down` kills
// the only thing that was watching, and the server then bills all night. An
// explicit "I'm done" is also a better signal than any traffic inference —
// there is nothing to infer.
//
// Deliberately idempotent and silent when there is nothing to destroy: `mole
// down` re-execs itself through sudo when it finds root-owned processes, so
// this runs twice, and the second pass must not turn a successful teardown into
// an error. It runs before that re-exec because the Vultr key lives in the
// user's environment, which sudo drops — the same reason `mole up` does its
// recoveries there.
func destroyActiveServerForDown() error {
	servers, err := loadServers()
	if err != nil {
		return fmt.Errorf("read local servers: %w", err)
	}
	for _, s := range servers {
		if !s.Active {
			continue
		}
		fmt.Printf("🗑️  Destroying %s (it bills until destroyed, not until stopped)...\n", s.Name)
		return destroyServerByName(s.Name, false)
	}
	// No active server in local state. But the running daemon may still owe a
	// destroy it could not pay — e.g. the lid-close DELETE failed and the local
	// entry was later lost, leaving a VPS that is STILL BILLING on Vultr with no
	// local entry to destroy it through. Every normal destroy route needs that
	// entry + the hy2 password, so don't return silently and let `mole down`
	// print "✅": say where the orphan actually goes.
	if owed := owedLidDestroyMsg(); owed != "" {
		fmt.Printf("⚠️  %s\n", owed)
		fmt.Println("    `mole down --destroy` cannot reach it — there is no local server entry to destroy.")
		fmt.Println("    Destroy the orphan with: `mole server reconcile --destroy-remote`")
	}
	return nil
}

// owedLidDestroyMsg returns a one-line description of a destroy that fired at a
// lid close and was never paid, or "" when there is no such debt. It is the same
// record `mole status` surfaces, read here so `mole down --destroy` can refuse
// to go quietly when the owed server is unreachable.
func owedLidDestroyMsg() string {
	debt := utils.ReadLidDebt()
	if debt == nil {
		return ""
	}
	who := debt.Server
	if who == "" {
		who = "the server"
	}
	switch debt.State {
	case utils.LidDestroyCancelled:
		return fmt.Sprintf("A destroy owed since %s was abandoned when the lid opened and %s is STILL BILLING",
			debt.Since.Format("15:04"), who)
	default:
		return fmt.Sprintf("A lid close at %s failed to destroy %s (%d attempts) and it is STILL BILLING",
			debt.Since.Format("2006-01-02 15:04"), who, debt.Attempts)
	}
}

// runDown drives the system back to a clean state and only prints success once
// it has *verified* nothing is left. The previous implementation trusted the
// PID file, killed at most one process, and printed "✅ VPN stopped" even when
// SIGKILL didn't take — which left orphan daemons that broke the next mole up.
//
// Flow:
//  1. Collect every candidate: PID file + ps scan for `mole up --internal-daemon`,
//     plus every sing-box currently bound to our config.
//  2. If anything is owned by root and we aren't, re-exec via sudo.
//  3. SIGTERM all daemons in parallel, wait once.
//  4. SIGKILL stragglers, wait once.
//  5. Kill any sing-box still pointing at our config (no ppid filter — the
//     daemons are dead, so anything bound to our config is leftover).
//  6. Restore host state (DNS / UDP buffers / pid / state files).
//  7. Re-scan ps; only print success if nothing is left.
func runDown(cmd *cobra.Command, args []string) error {
	// Before the sudo hop below, because the Vultr key lives in the user's
	// environment and sudo drops it.
	//
	// It used to also matter that this ran before the tunnel went away — the
	// API call rode the proxy, so it wanted a working tunnel underneath it.
	// That is no longer true and was never a good property: it made tearing
	// down the server depend on the thing being torn down. The management call
	// now goes out the physical interface (utils.NewBoundHTTPClient) through
	// the kill switch's exemption, so it works with the tunnel up, down, or
	// dark.
	if destroyOnDown {
		if err := destroyActiveServerForDown(); err != nil {
			return fmt.Errorf("destroy on down: %w (the VPN was NOT stopped; fix or re-run without --destroy)", err)
		}
	}

	// Only when --destroy was not given, which just settled the debt itself. A
	// destroy that fired at a closing lid and failed leaves a server running
	// that nobody is watching; stopping the daemon removes the last thing that
	// was going to retry it, so say so here — and, since the record now outlives
	// state.json, `mole status` keeps saying it afterwards.
	if !destroyOnDown {
		printLeftoverLidDebt()
	}

	fmt.Println("🛑 Stopping VPN...")

	daemons := collectMoleDaemons()
	singboxes := collectOurSingboxes()

	if len(daemons) == 0 && len(singboxes) == 0 {
		// Nothing alive — still tidy up any host state a previous unclean
		// exit may have left behind. That includes a kill switch armed by a
		// SIGKILL'd daemon: it is fail-closed by design, and this is exactly
		// the "next mole down" that disarms it.
		if utils.KillSwitchLoaded() {
			utils.UnloadKillSwitch()
		}
		utils.RestoreDNS()
		utils.RestoreNetworkStack()
		utils.RemovePID()
		utils.RemoveState()
		fmt.Println("✅ VPN stopped")
		return nil
	}

	// Anything we found that's owned by root needs root to signal. The daemon
	// is normally root (it owns TUN); sing-box inherits that. If we're not
	// root, hop through sudo so signals actually land.
	if needsRoot(daemons, singboxes) && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") && os.Geteuid() != 0 {
		return reExecDownViaSudo()
	}

	// SIGTERM every daemon, wait collectively. Killing daemons (not sing-box)
	// first lets each daemon's deferred Stop()/RestoreDNS() run on the happy
	// path before we resort to broader cleanup.
	for _, pid := range daemons {
		signalPID(pid, syscall.SIGTERM)
	}
	waitAllExit(daemons, 10*time.Second)

	// SIGKILL anyone that ignored SIGTERM.
	for _, pid := range daemons {
		if utils.IsAlive(pid) {
			signalPID(pid, syscall.SIGKILL)
		}
	}
	waitAllExit(daemons, 3*time.Second)

	// Daemons should be gone — anything still holding our config path is
	// leftover. Sweep wide (no ppid filter) since there's no live daemon
	// whose child we could mistake for an orphan.
	for _, pid := range core.FindAllSingboxesByConfig(utils.SingboxConfigPath()) {
		core.KillSingboxPID(pid)
	}
	// Also catch the state.json-recorded sing-box, in case the config path
	// changed between runs.
	core.KillOrphanedSingboxes(utils.SingboxConfigPath())

	// The daemon's own deferred unload only runs on orderly exit; after a
	// SIGKILL path the anchor is still armed. Unload is idempotent, so just
	// always disarm here.
	utils.UnloadKillSwitch()
	utils.RestoreDNS()
	utils.RestoreNetworkStack()
	utils.RemovePID()
	utils.RemoveState()

	// Final verification — the whole point of this rewrite. Don't lie to the
	// user; if something survived, surface it with PIDs and exit non-zero so
	// `mole down && mole up` short-circuits instead of starting on dirty state.
	leftoverDaemons := collectMoleDaemons()
	leftoverSingboxes := collectOurSingboxes()
	if len(leftoverDaemons) > 0 || len(leftoverSingboxes) > 0 {
		var parts []string
		if len(leftoverDaemons) > 0 {
			parts = append(parts, fmt.Sprintf("mole daemon pids=%v", leftoverDaemons))
		}
		if len(leftoverSingboxes) > 0 {
			parts = append(parts, fmt.Sprintf("sing-box pids=%v", leftoverSingboxes))
		}
		return fmt.Errorf("VPN not fully stopped: %s (try `sudo kill -9 <pid>` and re-run)", strings.Join(parts, ", "))
	}

	fmt.Println("✅ VPN stopped")
	return nil
}

// collectMoleDaemons returns the deduped sorted set of pids that look like a
// mole daemon: the recorded PID file (if it points to a live mole) plus every
// `mole up --internal-daemon` reported by pgrep. Two sources because the PID
// file can be stale, and pgrep can miss processes whose argv was modified.
func collectMoleDaemons() []int {
	seen := map[int]bool{}
	add := func(pid int) {
		if pid > 0 && utils.IsAlive(pid) {
			seen[pid] = true
		}
	}

	if pid, err := utils.ReadPID(); err == nil {
		// Don't gate on IsRunning here — if the PID file points at a live
		// process that *isn't* a mole daemon (PID reuse), we still want to
		// drop the stale file at the end, but we shouldn't kill the stranger.
		// IsRunning's exe check covers that.
		if utils.IsRunning(pid) {
			add(pid)
		}
	}

	for _, pid := range pgrepAll("mole up --internal-daemon") {
		add(pid)
	}

	pids := make([]int, 0, len(seen))
	for p := range seen {
		pids = append(pids, p)
	}
	sort.Ints(pids)
	return pids
}

// collectOurSingboxes returns every sing-box pid currently running with our
// config path, plus the one recorded in state.json. No ppid filter — see
// runDown header for why that's safe at this point.
func collectOurSingboxes() []int {
	seen := map[int]bool{}
	for _, pid := range core.FindAllSingboxesByConfig(utils.SingboxConfigPath()) {
		if pid > 0 && utils.IsAlive(pid) {
			seen[pid] = true
		}
	}
	if st, err := utils.ReadState(); err == nil && st.SingboxPID > 0 && utils.IsAlive(st.SingboxPID) {
		seen[st.SingboxPID] = true
	}
	pids := make([]int, 0, len(seen))
	for p := range seen {
		pids = append(pids, p)
	}
	sort.Ints(pids)
	return pids
}

// needsRoot reports whether any of the listed pids is owned by root, which
// means a non-root caller can't signal them and must re-exec via sudo.
func needsRoot(groups ...[]int) bool {
	for _, g := range groups {
		for _, pid := range g {
			if isRootPID(pid) {
				return true
			}
		}
	}
	return false
}

func isRootPID(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "uid=").Output()
	if err != nil {
		// Couldn't tell — assume root to err on the side of escalating; the
		// worst case is a redundant sudo prompt.
		return true
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return true
	}
	return uid == 0
}

func signalPID(pid int, sig syscall.Signal) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(sig)
}

// waitAllExit blocks until every pid is gone or the deadline fires. Polls at
// 100ms — fast enough that an orderly SIGTERM exits within one tick after the
// process actually quits, slow enough to not chew CPU.
func waitAllExit(pids []int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		anyAlive := false
		for _, pid := range pids {
			if utils.IsAlive(pid) {
				anyAlive = true
				break
			}
		}
		if !anyAlive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pgrepAll returns every pid whose full argv matches the pattern. Uses
// `pgrep -f` because our daemon's comm is just "mole" and we need the
// "--internal-daemon" suffix to distinguish it from a user-run mole CLI.
func pgrepAll(pattern string) []int {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		// pgrep exits 1 when there are no matches — treat as empty list.
		return nil
	}
	var pids []int
	self := os.Getpid()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func reExecDownViaSudo() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Drop --destroy: it was already carried out in the pre-sudo pass, which is
	// the only one with the Vultr key. Passing it on would re-enter the destroy
	// with the *next* server promoted to active and delete that one too — the
	// idempotence of "destroy the active server" holds only for the last one
	// standing.
	args := []string{exe}
	for _, a := range os.Args[1:] {
		if a == "--destroy" {
			continue
		}
		args = append(args, a)
	}
	c := exec.Command("sudo", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
