package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PIDInfo is the on-disk pid file payload. The exe path lets IsRunning
// distinguish a legitimate live mole daemon from an unrelated process that
// happens to have inherited the same PID after reuse.
type PIDInfo struct {
	PID int    `json:"pid"`
	Exe string `json:"exe"`
}

// WritePID writes the current process pid plus its executable path so we can
// fingerprint-check it later.
func WritePID() error {
	exe, _ := os.Executable()
	info := PIDInfo{PID: os.Getpid(), Exe: exe}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := PIDPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, PIDPath())
}

// ReadPID returns the recorded pid. Accepts both the new JSON format and the
// legacy plain-int format so an in-flight upgrade doesn't strand users.
func ReadPID() (int, error) {
	info, err := readPIDInfo()
	if err != nil {
		return 0, err
	}
	return info.PID, nil
}

func readPIDInfo() (PIDInfo, error) {
	data, err := os.ReadFile(PIDPath())
	if err != nil {
		return PIDInfo{}, err
	}
	var info PIDInfo
	if json.Unmarshal(data, &info) == nil && info.PID > 0 {
		return info, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return PIDInfo{}, err
	}
	return PIDInfo{PID: pid}, nil
}

// RemovePID removes the PID file.
func RemovePID() error { return os.Remove(PIDPath()) }

// IsAlive reports whether *any* process with the given pid is currently
// running. Use this when you don't care what the process is (e.g., waiting
// for a sing-box pid we recorded earlier to actually exit).
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// IsRunning reports whether the given pid is alive AND looks like the mole
// daemon recorded in the pid file. This guards against pid reuse: after a
// reboot or long uptime, the recorded pid may now belong to an unrelated
// process; without this check `mole down` would signal that stranger and
// `mole up` would refuse to start.
func IsRunning(pid int) bool {
	if !IsAlive(pid) {
		return false
	}
	info, err := readPIDInfo()
	if err != nil || info.PID != pid {
		// Legacy file (no exe recorded) — fall back to alive check; safer than
		// refusing to recognize a real running daemon.
		return true
	}
	if info.Exe == "" {
		return true
	}
	return processMatchesExe(pid, info.Exe)
}

// processMatchesExe checks whether the running pid's command name matches the
// expected executable. Uses `ps -o comm=`, which works on macOS and Linux
// without /proc. Returns true on inconclusive results to avoid false-negatives
// (we only want to rule out clearly-different processes).
func processMatchesExe(pid int, expectedExe string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return true
	}
	actual := strings.TrimSpace(string(out))
	if actual == "" {
		return true
	}
	want := filepath.Base(expectedExe)
	// `ps comm=` may give the basename or a longer path. Match by basename on
	// either side; deliberately do NOT fall back to a generic "mole" substring,
	// which lit up unrelated processes containing "mole" after PID reuse.
	return strings.Contains(actual, want) || filepath.Base(actual) == want
}

// CheckAlreadyRunning checks if another mole instance is running.
func CheckAlreadyRunning() error {
	pid, err := ReadPID()
	if err != nil {
		return nil
	}
	if IsRunning(pid) {
		return fmt.Errorf("mole already running (pid=%d). Use 'mole down' to stop it first", pid)
	}
	// Stale (process gone, or pid reused by something that isn't mole).
	RemovePID()
	return nil
}
