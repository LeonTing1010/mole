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
//
// net.link.generic.system.flow_advisory was tried here on 2026-08-22 and is
// deliberately NOT in this list. Recording the negative result so it does not
// get re-derived: the theory was that ENOBUFS on the hy2 socket came from the
// interface AQM (FQ_CODEL) pushing back on a sender outrunning the link, and
// that turning the advisory off would make the kernel DROP the datagram instead
// of failing the write — which matters because quic-go destroys the whole QUIC
// connection on any write error but EMSGSIZE (sagernet/quic-go send_queue.go),
// so one congested moment resets every stream.
//
// The mechanism is real; the fix was not. With flow_advisory=0 in force, an
// evening on a degraded link (-72 dBm, Wi-Fi txRate 14-47 Mbps against a 65
// Mbps ceiling) still produced 45 ENOBUFS in 45 minutes — the same rate as the
// 1787-per-day baseline it was meant to remove. The write failures evidently
// come from the socket send buffer, not the flow advisory. A system-wide knob
// with no measurable benefit is not worth holding, so it was reverted rather
// than left in as a hopeful no-op.
var netTuning = []struct{ Key, Value string }{
	{"kern.ipc.maxsockbuf", "8388608"},
	{"net.inet.udp.recvspace", "7168000"},
	{"net.inet.udp.sendspace", "7168000"},
	{"net.inet.udp.maxdgram", "65535"},
}

func sysctlBackupPath() string { return filepath.Join(MoleDir(), "sysctl-backup.json") }

// TuneNetworkStack applies the sysctls in netTuning — UDP socket buffer limits
// for high-throughput QUIC, and the interface flow-advisory switch — backing up
// the previous values first. No-op on non-macOS platforms, and idempotent.
func TuneNetworkStack() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	// Never re-read a key we already have a backup for. A leftover backup from a
	// killed run holds the true pre-tuning value; re-reading now would record the
	// already-applied value as the "original" and a later RestoreNetworkStack
	// would be a no-op that leaves the change in place permanently.
	//
	// Keys absent from an existing backup are still captured, because that is
	// what a backup written by an older mole looks like after this list grows —
	// and a key that never gets backed up never gets restored.
	backup := map[string]string{}
	if data, err := os.ReadFile(sysctlBackupPath()); err == nil {
		_ = json.Unmarshal(data, &backup)
	}
	dirty := false
	for _, kv := range netTuning {
		if _, ok := backup[kv.Key]; ok {
			continue
		}
		if v, err := readSysctl(kv.Key); err == nil {
			backup[kv.Key] = v
			dirty = true
		}
	}
	if dirty {
		data, _ := json.MarshalIndent(backup, "", "  ")
		_ = os.WriteFile(sysctlBackupPath(), data, 0644)
	}

	var failed []string
	for _, kv := range netTuning {
		// Skip OIDs that don't exist on this kernel. net.inet.udp.sendspace is
		// FreeBSD-only and absent on modern macOS (the UDP send buffer is bounded
		// by kern.ipc.maxsockbuf, already raised above), so trying to set it just
		// produced a misleading "UDP buffer tuning failed" warning on every up.
		if _, err := readSysctl(kv.Key); err != nil {
			continue
		}
		if err := writeSysctl(kv.Key, kv.Value); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", kv.Key, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("sysctl: %s", strings.Join(failed, "; "))
	}
	return nil
}

// RestoreNetworkStack rolls back what TuneNetworkStack applied. Safe to call
// repeatedly; silently does nothing if no backup exists.
func RestoreNetworkStack() {
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
