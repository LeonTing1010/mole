package utils

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// WritePID writes the current process PID to file
func WritePID() error {
	pid := os.Getpid()
	return os.WriteFile(PIDPath(), []byte(strconv.Itoa(pid)), 0644)
}

// ReadPID reads the PID from file
func ReadPID() (int, error) {
	data, err := os.ReadFile(PIDPath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// RemovePID removes the PID file
func RemovePID() error {
	return os.Remove(PIDPath())
}

// IsRunning checks if the process with the given PID is running
func IsRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds, need to send signal 0 to check
	err = process.Signal(os.Signal(nil))
	return err == nil
}

// CheckAlreadyRunning checks if another mole instance is running
func CheckAlreadyRunning() error {
	pid, err := ReadPID()
	if err != nil {
		// No PID file, not running
		return nil
	}

	if IsRunning(pid) {
		return fmt.Errorf("mole already running (pid=%d). Use 'mole down' to stop it first", pid)
	}

	// Stale PID file, remove it
	RemovePID()
	return nil
}
