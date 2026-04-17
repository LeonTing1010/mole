package utils

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Daemonize re-executes the current binary with an extra flag so the child
// knows to run the daemon path, detaches from the controlling terminal, and
// routes stdout/stderr to the mole log file. Returns the child PID.
func Daemonize(extraArg string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate executable: %w", err)
	}

	// Append extraArg after the original arguments; cobra accepts flags in any
	// position relative to the subcommand.
	args := append([]string{}, os.Args[1:]...)
	args = append(args, extraArg)

	logFile, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		logFile.Close()
		return 0, fmt.Errorf("open /dev/null: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn daemon: %w", err)
	}

	// Parent releases the child; Release avoids a zombie if the child
	// outlives the short-lived parent.
	if err := cmd.Process.Release(); err != nil {
		return cmd.Process.Pid, fmt.Errorf("release daemon: %w", err)
	}
	return cmd.Process.Pid, nil
}
