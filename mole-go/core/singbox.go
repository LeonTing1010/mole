package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/leo/mole/utils"
)

var (
	currentProcess *exec.Cmd
	processMutex   sync.Mutex
	serverAddress  string
)

// Start launches sing-box with the given configuration.
func Start(configPath string) error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess != nil {
		return fmt.Errorf("VPN is already running")
	}

	singboxPath, err := findSingbox()
	if err != nil {
		return err
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		// TUN requires root on macOS.
		cmd = exec.Command("sudo", singboxPath, "run", "-c", configPath)
	} else {
		cmd = exec.Command(singboxPath, "run", "-c", configPath)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true",
		"ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true",
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start sing-box: %w", err)
	}
	currentProcess = cmd

	time.Sleep(2 * time.Second)
	if currentProcess.ProcessState != nil && currentProcess.ProcessState.Exited() {
		currentProcess = nil
		return fmt.Errorf("sing-box exited unexpectedly")
	}
	return nil
}

// Stop terminates the running sing-box process.
func Stop() error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess == nil {
		killExistingSingbox()
		return nil
	}

	if err := currentProcess.Process.Signal(os.Interrupt); err != nil {
		if err := currentProcess.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop sing-box: %w", err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- currentProcess.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		currentProcess.Process.Kill()
	}
	currentProcess = nil
	return nil
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
