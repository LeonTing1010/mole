package core

import (
	"context"
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

// Start launches Hiddify-core with the given configuration
func Start(configPath string) error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess != nil {
		return fmt.Errorf("VPN is already running")
	}

	// Find HiddifyCli binary
	hiddifyPath, err := findHiddifyCli()
	if err != nil {
		return err
	}

	// Start HiddifyCli process
	cmd := exec.Command(hiddifyPath, "run", "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start HiddifyCli: %w", err)
	}

	currentProcess = cmd
	startTime = time.Now()

	// Wait a moment to check if process started successfully
	time.Sleep(2 * time.Second)
	if currentProcess.ProcessState != nil && currentProcess.ProcessState.Exited() {
		currentProcess = nil
		return fmt.Errorf("HiddifyCli exited unexpectedly")
	}

	return nil
}

// Stop terminates the running Hiddify-core process
func Stop() error {
	processMutex.Lock()
	defer processMutex.Unlock()

	if currentProcess == nil {
		// Try to kill any existing HiddifyCli processes
		killExistingHiddify()
		return nil
	}

	// Try graceful shutdown first
	if err := currentProcess.Process.Signal(os.Interrupt); err != nil {
		// Force kill if graceful shutdown fails
		if err := currentProcess.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop HiddifyCli: %w", err)
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

// findHiddifyCli locates the HiddifyCli binary
func findHiddifyCli() (string, error) {
	// 1. Check ~/.mole/bin/HiddifyCli (installed by mole)
	moleHiddify := utils.HiddifyCliPath()
	if _, err := os.Stat(moleHiddify); err == nil {
		return moleHiddify, nil
	}

	// 2. Check if HiddifyCli is in PATH
	if path, err := exec.LookPath("HiddifyCli"); err == nil {
		return path, nil
	}

	// 3. Check common installation paths
	paths := []string{
		"/usr/local/bin/HiddifyCli",
		"/usr/bin/HiddifyCli",
		"/opt/hiddify/HiddifyCli",
	}

	// 4. Add platform-specific paths
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths,
			"/Applications/Hiddify.app/Contents/MacOS/HiddifyCli",
			filepath.Join(os.Getenv("HOME"), "Applications/Hiddify.app/Contents/MacOS/HiddifyCli"),
		)
	case "windows":
		paths = append(paths,
			`C:\Program Files\Hiddify\HiddifyCli.exe`,
			`C:\Program Files (x86)\Hiddify\HiddifyCli.exe`,
		)
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("HiddifyCli not found. Run 'mole install' or install from https://hiddify.com")
}

// killExistingHiddify kills any running HiddifyCli processes
func killExistingHiddify() {
	switch runtime.GOOS {
	case "darwin", "linux":
		exec.Command("pkill", "-f", "HiddifyCli").Run()
	case "windows":
		exec.Command("taskkill", "/F", "/IM", "HiddifyCli.exe").Run()
	}
}

// SetServerAddress sets the server address for status display
func SetServerAddress(address string) {
	serverAddress = address
}

// MonitorLogs monitors and forwards Hiddify logs
func MonitorLogs(ctx context.Context, logFile string) error {
	// Implementation for log monitoring
	// This could tail the log file and format output
	return nil
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

// GetHiddifyVersion returns the installed HiddifyCli version
func GetHiddifyVersion() (string, error) {
	hiddifyPath, err := findHiddifyCli()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(hiddifyPath, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}
