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

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the background VPN",
	RunE:  runDown,
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
	fmt.Println("🛑 Stopping VPN...")

	daemons := collectMoleDaemons()
	singboxes := collectOurSingboxes()

	if len(daemons) == 0 && len(singboxes) == 0 {
		// Nothing alive — still tidy up any host state a previous unclean
		// exit may have left behind.
		utils.RestoreDNS()
		utils.RestoreUDPBuffers()
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

	utils.RestoreDNS()
	utils.RestoreUDPBuffers()
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
	args := append([]string{exe}, os.Args[1:]...)
	c := exec.Command("sudo", args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
