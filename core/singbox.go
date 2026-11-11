package core

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

var (
	currentProcess *exec.Cmd
	exitCh         chan struct{}
	exitErr        error
	processMutex   sync.Mutex
)

// Start launches sing-box with the given configuration.
// It checks process survival and, for full configs, real direct and FakeIP DNS answers.
func Start(configPath string) error {
	processMutex.Lock()

	if currentProcess != nil {
		processMutex.Unlock()
		return fmt.Errorf("VPN is already running")
	}

	singboxPath, err := findSingbox()
	if err != nil {
		processMutex.Unlock()
		return err
	}

	if err := checkEngineConfig(singboxPath, configPath); err != nil {
		processMutex.Unlock()
		return err
	}

	if err := recoverFakeIPCache(configPath); err != nil {
		processMutex.Unlock()
		return fmt.Errorf("preserve FakeIP cache before engine restart: %w", err)
	}

	fmt.Printf("🔧 sing-box: %s\n", singboxPath)
	if out, err := exec.Command(singboxPath, "version").Output(); err == nil {
		fmt.Printf("   %s\n", strings.SplitN(string(out), "\n", 2)[0])
	}

	var cmd *exec.Cmd
	// TUN requires root. If we're already root (daemon re-execed via sudo),
	// skip wrapping with sudo — would fail in a detached context with no TTY.
	if (runtime.GOOS == "darwin" || runtime.GOOS == "linux") && os.Geteuid() != 0 {
		cmd = exec.Command("sudo", singboxPath, "run", "-c", configPath)
	} else {
		cmd = exec.Command(singboxPath, "run", "-c", configPath)
	}
	fmt.Printf("🔧 exec: %s\n", strings.Join(cmd.Args, " "))
	logFile, err := os.OpenFile(utils.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		processMutex.Unlock()
		return fmt.Errorf("open log file: %w", err)
	}
	// Splice the flood sensor into sing-box's output instead of tailing the log
	// file, which rotates. Assigning the *same* writer to both streams matters:
	// os/exec reuses one pipe when Stdout and Stderr are the same value, so
	// lines cannot interleave mid-write the way two independent copiers would.
	sink := io.MultiWriter(logFile, &singboxFlood, &singboxNodeDials)
	cmd.Stdout = sink
	cmd.Stderr = sink
	cmd.Env = engineEnv()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		processMutex.Unlock()
		return fmt.Errorf("failed to start sing-box: %w", err)
	}
	currentProcess = cmd
	exitCh = make(chan struct{})
	exitErr = nil
	currentExitCh := exitCh
	processMutex.Unlock()

	go func(c *exec.Cmd, done chan struct{}, lf *os.File) {
		err := c.Wait()
		lf.Close()
		processMutex.Lock()
		exitErr = err
		if currentProcess == c {
			currentProcess = nil
		}
		close(done)
		processMutex.Unlock()
	}(cmd, currentExitCh, logFile)

	// Early-exit guard: fail fast if config is invalid.
	select {
	case <-currentExitCh:
		return fmt.Errorf("sing-box exited unexpectedly (see %s)", utils.LogPath())
	case <-time.After(2 * time.Second):
	}
	addr, err := engineDNSAddress(configPath)
	if err == nil && addr != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err = waitDNSReady(ctx, addr, []string{"www.baidu.com", "example.com"}, currentExitCh)
		cancel()
	}
	if err != nil {
		Stop()
		return fmt.Errorf("engine DNS not ready: %w", err)
	}

	return nil
}

// SingboxPID returns the pid of the sing-box child process, or 0 if none is
// currently tracked. The supervisor stamps this into state.json so that the
// `mole down` parent (a different process) can kill the right sing-box on the
// SIGKILL escalation path instead of carpet-bombing the system with `pkill`.
func SingboxPID() int {
	processMutex.Lock()
	defer processMutex.Unlock()
	if currentProcess == nil || currentProcess.Process == nil {
		return 0
	}
	return currentProcess.Process.Pid
}

// Stop terminates the running sing-box process. Only meaningful in the
// process that called Start (the daemon). Calling it from a different process
// is a no-op — use the SingboxPID recorded in state.json to kill the child
// directly instead.
func Stop() error {
	processMutex.Lock()
	if currentProcess == nil {
		processMutex.Unlock()
		return nil
	}
	proc := currentProcess.Process
	done := exitCh
	processMutex.Unlock()

	if err := proc.Signal(os.Interrupt); err != nil {
		if err := proc.Kill(); err != nil {
			return fmt.Errorf("failed to stop sing-box: %w", err)
		}
	}

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
	}

	// SIGINT did not land within the grace window. SIGKILL reaches almost
	// everything — but not a child wedged in a syscall it never returns from,
	// e.g. a TUN/IOKit read frozen across a night of sleep, or a UDP socket
	// left in the "no buffer space available" state the log showed on the night
	// the lid-close destroy hung the daemon. The wait after Kill used to be
	// unbounded, and that was a real bug, not a theoretical one: Stop() runs
	// synchronously on the supervisor's own shutdown goroutine, so a Stop()
	// that never returns means Run() never returns, and the daemon becomes a
	// zombie that keeps the PF kill switch armed and state.json frozen until
	// someone runs `mole down`. Bound the wait, say what was left behind, and
	// let the next `mole down` sweep the orphan.
	if err := proc.Kill(); err != nil {
		log.Printf("sing-box did not exit after SIGINT; SIGKILL failed: %v", err)
		return fmt.Errorf("sing-box did not exit: %w", err)
	}
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		log.Printf("sing-box is wedged and ignoring SIGKILL — leaving it to `mole down`'s orphan sweep")
		return fmt.Errorf("sing-box is wedged and ignoring SIGKILL")
	}
}

// ExitChan returns a channel that closes when the current sing-box process exits.
// Returns nil if nothing is running.
func ExitChan() <-chan struct{} {
	processMutex.Lock()
	defer processMutex.Unlock()
	return exitCh
}

// ExitError returns the last exit error (valid after ExitChan closes).
func ExitError() error {
	processMutex.Lock()
	defer processMutex.Unlock()
	return exitErr
}

// KillOrphanedSingboxes finds and kills sing-box processes that mole started
// but no longer owns. Two-stage:
//  1. The pid recorded in state.json (fast, exact — handles the common case
//     where the previous daemon got SIGKILL'd before its defers ran).
//  2. A ps(1) sweep for sing-box processes whose command line contains our
//     config path AND whose parent pid is 1 (orphaned). Catches the cases
//     state.json misses: corrupt/missing state, never-written state from a
//     very-early crash, or accumulated leakage across multiple incidents.
//
// Identification is deliberately conservative: only kill sing-box instances
// running our specific config path, and only when reparented to init/launchd.
// This avoids stomping on unrelated sing-box deployments the user may have
// running with a different config.
//
// MUST only be called when no live mole daemon exists — otherwise we risk
// killing a healthy daemon's child. Callers gate on CheckAlreadyRunning.
func KillOrphanedSingboxes(configPath string) {
	killed := map[int]bool{}

	identified := map[int]bool{}
	for _, pid := range FindAllSingboxesByConfig(configPath) {
		identified[pid] = true
	}
	if st, err := utils.ReadState(); err == nil && identified[st.SingboxPID] {
		if killSingboxPID(st.SingboxPID) {
			killed[st.SingboxPID] = true
		}
	}

	for _, pid := range findOrphanSingboxesByPS(configPath) {
		if killed[pid] {
			continue
		}
		killSingboxPID(pid)
	}
}

// killSingboxPID best-effort terminates pid: SIGTERM, wait up to 3s, then
// SIGKILL. Returns true if a live process was found at the start.
func killSingboxPID(pid int) bool {
	if !utils.IsAlive(pid) || !isSingboxProcess(pid) {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Signal(syscall.SIGTERM)
	for i := 0; i < 30 && utils.IsAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if utils.IsAlive(pid) && isSingboxProcess(pid) {
		_ = p.Signal(syscall.SIGKILL)
	}
	return true
}

// isSingboxProcess fails closed when the process identity cannot be read.
func isSingboxProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	return err == nil && filepath.Base(strings.TrimSpace(string(out))) == "sing-box"
}

// findOrphanSingboxesByPS returns pids of sing-box processes that match our
// config path and have been reparented to init (ppid == 1).
func findOrphanSingboxesByPS(configPath string) []int {
	return scanSingboxByPS(configPath, true)
}

// FindAllSingboxesByConfig returns pids of every sing-box process running with
// our config path, regardless of parent. Used by `mole down` after the daemon
// is dead, when there is no risk of stomping on a healthy daemon's child:
// anything still pointing at our config is leftover and must die for the next
// `mole up` to reclaim TUN/UDP/the Clash API port cleanly.
func FindAllSingboxesByConfig(configPath string) []int {
	return scanSingboxByPS(configPath, false)
}

func scanSingboxByPS(configPath string, orphansOnly bool) []int {
	if runtime.GOOS == "windows" {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	self := os.Getpid()
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if orphansOnly && ppid != 1 {
			continue
		}
		cmd := strings.Join(fields[2:], " ")
		if !strings.Contains(cmd, "sing-box") {
			continue
		}
		if !commandUsesConfig(cmd, configPath) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// commandUsesConfig checks a complete configuration argument.
func commandUsesConfig(command, configPath string) bool {
	for _, flag := range []string{" -c ", " --config ", " -c=", " --config="} {
		_, rest, found := strings.Cut(command, flag)
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		for _, path := range []string{configPath, "\"" + configPath + "\"", "'" + configPath + "'"} {
			if rest == path || strings.HasPrefix(rest, path+" ") {
				return true
			}
		}
	}
	return false
}

// KillSingboxPID is the exported best-effort SIGTERM-then-SIGKILL helper used
// by `mole down` to clean up sing-boxes still bound to our config after the
// daemon is gone.
func KillSingboxPID(pid int) bool { return killSingboxPID(pid) }

func findSingbox() (string, error) {
	moleSingbox := filepath.Join(utils.BinDir(), "sing-box")
	if _, err := os.Stat(moleSingbox); err == nil {
		return moleSingbox, nil
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		return path, nil
	}
	for _, p := range []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box", "/opt/sing-box/sing-box"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("sing-box not found — reinstall mole")
}
