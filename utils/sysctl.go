package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// macOS' default UDP socket buffers are tiny (≈42KB) which caps hy2/QUIC
// throughput and causes ENOBUFS ("no buffer space available") on large
// uploads. Order matters: kern.ipc.maxsockbuf is the per-direction
// ceiling for recv/sendspace, so it must be raised first or the
// subsequent writes get silently clamped.
var udpTuning = []struct{ Key, Value string }{
	{"kern.ipc.maxsockbuf", "8388608"},
	{"net.inet.udp.recvspace", "7168000"},
	{"net.inet.udp.sendspace", "7168000"},
	{"net.inet.udp.maxdgram", "65535"},
}

func sysctlBackupPath() string { return filepath.Join(MoleDir(), "sysctl-backup.json") }

// TuneUDPBuffers raises UDP socket buffer limits for high-throughput QUIC
// and backs up previous values. No-op on non-macOS platforms and when
// already running with equal-or-larger limits (idempotent).
func TuneUDPBuffers() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	backup := make(map[string]string, len(udpTuning))
	for _, kv := range udpTuning {
		if v, err := readSysctl(kv.Key); err == nil {
			backup[kv.Key] = v
		}
	}
	data, _ := json.MarshalIndent(backup, "", "  ")
	_ = os.WriteFile(sysctlBackupPath(), data, 0644)

	var failed []string
	for _, kv := range udpTuning {
		if err := writeSysctl(kv.Key, kv.Value); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", kv.Key, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("sysctl: %s", strings.Join(failed, "; "))
	}
	return nil
}

// RestoreUDPBuffers rolls back what TuneUDPBuffers raised. Safe to call
// repeatedly; silently does nothing if no backup exists.
func RestoreUDPBuffers() {
	if runtime.GOOS != "darwin" {
		return
	}
	data, err := os.ReadFile(sysctlBackupPath())
	if err != nil {
		return
	}
	var backup map[string]string
	if json.Unmarshal(data, &backup) != nil {
		return
	}
	for k, v := range backup {
		_ = writeSysctl(k, v)
	}
	_ = os.Remove(sysctlBackupPath())
}

func readSysctl(key string) (string, error) {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func writeSysctl(key, value string) error {
	// sysctl -w requires root; the daemon already runs as root so no sudo wrap.
	return exec.Command("sysctl", "-w", key+"="+value).Run()
}
