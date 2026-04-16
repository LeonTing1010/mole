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

	"github.com/leo/mole/utils"
)

var (
	currentProcess *exec.Cmd
	processMutex   sync.Mutex
	startTime      time.Time
	serverAddress  string
)

// Status represents the VPN connection status
type Status struct {
	Running bool
	Server  string
	Uptime  string
	IPInfo  utils.IPInfo
}

// Start launches sing-box with the given configuration
func Start(configPath string) error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess != nil {
		return fmt.Errorf("VPN is already running")
	}

	// Find sing-box binary
	singboxPath, err := findSingbox()
	if err != nil {
		return err
	}

	// Start sing-box process with TUN mode
	cmd := exec.Command(singboxPath, "run", "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start sing-box: %w", err)
	}

	currentProcess = cmd
	startTime = time.Now()

	// Wait a moment to check if process started successfully
	time.Sleep(2 * time.Second)
	if currentProcess.ProcessState != nil && currentProcess.ProcessState.Exited() {
		currentProcess = nil
		return fmt.Errorf("sing-box exited unexpectedly")
	}

	return nil
}

// Stop terminates the running sing-box process
func Stop() error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess == nil {
		// Try to kill any existing sing-box processes
		killExistingSingbox()
		return nil
	}

	// Try graceful shutdown first
	if err := currentProcess.Process.Signal(os.Interrupt); err != nil {
		// Force kill if graceful shutdown fails
		if err := currentProcess.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop sing-box: %w", err)
		}
	}

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- currentProcess.Wait()
	}()

	select {
	case <-done:
		// Process exited
	case <-time.After(5 * time.Second):
		// Timeout, force kill
		currentProcess.Process.Kill()
	}

	currentProcess = nil
	return nil
}

// GetStatus returns the current VPN connection status
func GetStatus() (*Status, error) {
	processMutex.Lock()
	defer processMutex.Unlock()

	status := &Status{
		Running: currentProcess != nil,
		Server:  serverAddress,
	}

	if currentProcess != nil {
		// Check if process is still running
		if currentProcess.ProcessState != nil && currentProcess.ProcessState.Exited() {
			status.Running = false
			currentProcess = nil
		} else {
			status.Uptime = time.Since(startTime).Round(time.Second).String()
			if serverAddress != "" {
				info, _ := utils.GetIPInfo(serverAddress)
				status.IPInfo = info
			}
		}
	}

	return status, nil
}

// findSingbox locates the sing-box binary
func findSingbox() (string, error) {
	// 1. Check ~/.mole/bin/sing-box (installed by mole)
	moleSingbox := filepath.Join(utils.BinDir(), "sing-box")
	if _, err := os.Stat(moleSingbox); err == nil {
		return moleSingbox, nil
	}

	// 2. Check if sing-box is in PATH
	if path, err := exec.LookPath("sing-box"); err == nil {
		return path, nil
	}

	// 3. Check common installation paths
	paths := []string{
		"/usr/local/bin/sing-box",
		"/usr/bin/sing-box",
		"/opt/sing-box/sing-box",
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("sing-box not found. Please install sing-box or run 'mole install'")
}

// killExistingSingbox kills any running sing-box processes
func killExistingSingbox() {
	switch runtime.GOOS {
	case "darwin", "linux":
		exec.Command("pkill", "-f", "sing-box").Run()
	case "windows":
		exec.Command("taskkill", "/F", "/IM", "sing-box.exe").Run()
	}
}

// SetServerAddress sets the server address for status display
func SetServerAddress(address string) {
	serverAddress = address
}

// IsRunning returns true if the VPN is currently running
func IsRunning() bool {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess == nil {
		return false
	}

	if currentProcess.ProcessState != nil && currentProcess.ProcessState.Exited() {
		currentProcess = nil
		return false
	}

	return true
}

// GetSingboxVersion returns the installed sing-box version
func GetSingboxVersion() (string, error) {
	singboxPath, err := findSingbox()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(singboxPath, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}
