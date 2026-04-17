package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

var (
	currentProcess *exec.Cmd
	exitCh         chan struct{}
	exitErr        error
	processMutex   sync.Mutex
	serverAddress  string
)

// Start launches sing-box with the given configuration.
// It returns once the process has stayed alive for 2 seconds (basic config smoke test).
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
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(),
		"ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true",
		"ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true",
	)

	if err := cmd.Start(); err != nil {
		processMutex.Unlock()
		return fmt.Errorf("failed to start sing-box: %w", err)
	}
	currentProcess = cmd
	exitCh = make(chan struct{})
	exitErr = nil
	currentExitCh := exitCh
	processMutex.Unlock()

	go func(c *exec.Cmd, done chan struct{}) {
		err := c.Wait()
		processMutex.Lock()
		exitErr = err
		if currentProcess == c {
			currentProcess = nil
		}
		close(done)
		processMutex.Unlock()
	}(cmd, currentExitCh)

	// Early-exit guard: fail fast if config is invalid.
	select {
	case <-currentExitCh:
		return fmt.Errorf("sing-box exited unexpectedly (see %s)", utils.LogPath())
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Stop terminates the running sing-box process.
func Stop() error {
	processMutex.Lock()
	if currentProcess == nil {
		processMutex.Unlock()
		killExistingSingbox()
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
	case <-time.After(5 * time.Second):
		proc.Kill()
		<-done
	}
	return nil
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

// SetServerAddress records which server is currently active (for diagnostics).
func SetServerAddress(addr string) { serverAddress = addr }

// ServerAddress returns the last set server address.
func ServerAddress() string { return serverAddress }

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

func killExistingSingbox() {
	switch runtime.GOOS {
	case "darwin", "linux":
		exec.Command("pkill", "-f", "sing-box").Run()
	case "windows":
		exec.Command("taskkill", "/F", "/IM", "sing-box.exe").Run()
	}
}
