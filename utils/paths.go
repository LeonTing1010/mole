package utils

import (
	"os"
	"path/filepath"
	"syscall"
)

// ChownToMoleDirOwner hands path back to whoever owns ~/.mole. No-op unless we
// are root.
//
// TUN needs root, so the daemon runs as root and everything it writes into the
// user's mole directory lands root-owned. For the files only it writes
// (state.json, the cached rule-sets) that is harmless. servers.json is
// different: the CLI writes it as the user, and the daemon writes it too now
// that idle-destroy removes the entry for the server it just deleted. A
// root-owned copy happens to keep working — the directory is the user's, so the
// CLI's write-temp-and-rename still succeeds — which is exactly what makes it a
// trap: it survives until the first code path that opens the file in place.
func ChownToMoleDirOwner(path string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(MoleDir())
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = os.Chown(path, int(st.Uid), int(st.Gid))
}

// MoleDir returns the mole directory (~/.mole).
func MoleDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic("cannot find home directory")
	}
	dir := filepath.Join(home, ".mole")
	os.MkdirAll(dir, 0755)
	return dir
}

// BinDir returns the mole bin directory (~/.mole/bin).
func BinDir() string {
	dir := filepath.Join(MoleDir(), "bin")
	os.MkdirAll(dir, 0755)
	return dir
}

// ServersPath is the JSON file storing deployed VPS metadata.
func ServersPath() string { return filepath.Join(MoleDir(), "servers.json") }

// LadderMemoryPath is the per-region record of what a path was measured to
// carry. It outlives the servers that taught us, which is the point: a destroy
// removes the entry, and without this the next deploy in the same region starts
// from the conservative defaults again.
func LadderMemoryPath() string { return filepath.Join(MoleDir(), "ladders.json") }

// SingboxConfigPath is where the generated sing-box config is written.
func SingboxConfigPath() string { return filepath.Join(MoleDir(), "sing-box-config.json") }

// PIDPath is the running mole process pid file.
func PIDPath() string { return filepath.Join(MoleDir(), "mole.pid") }

// LogPath is where sing-box output is captured.
func LogPath() string { return filepath.Join(MoleDir(), "mole.log") }

// StatePath is where the running supervisor writes its health state.
func StatePath() string { return filepath.Join(MoleDir(), "state.json") }

// VultrKeyPath is where `mole up --idle-destroy` stashes the Vultr API key so
// the daemon can reach it.
//
// It exists because of one detail of how mole starts: TUN needs root, so `mole
// up` re-execs itself through sudo, and sudo resets the environment — the
// daemon therefore never sees VULTR_API_KEY. Every other management call is
// made by the CLI in the user's own shell and does not care. Idle-destroy is
// the first action mole takes on its own, hours later, from inside the daemon.
//
// Written 0600 and only when idle-destroy is actually enabled, so the copy
// exists solely for the feature that needs it. It is still a second copy of a
// root-scoped credential on disk: worth knowing, and a reason to prefer a
// Vultr key restricted to instance management over the account-wide one.
func VultrKeyPath() string { return filepath.Join(MoleDir(), "vultr_key") }

// APIIPsPath is the cache of what management hostnames last resolved to
// outside the tunnel.
//
// It exists for the one call that cannot afford a DNS round trip: the destroy
// at a closing lid, which gets ~30 seconds and often gets them on a link that
// has already gone dark. Knowing where api.vultr.com lived an hour ago turns a
// 5-second timeout into a dial that either works or fails immediately.
func APIIPsPath() string { return filepath.Join(MoleDir(), "api-ips.json") }

// LidDebtPath is where a destroy that fired at a lid close and never completed
// is recorded — the server it failed to delete is still running and billing.
//
// A file of its own rather than a field in state.json, because state.json is
// deleted by `mole down` and rewritten by the next `mole up`: on 2026-08-14 a
// destroy owed since 03:40 survived six hours of dark wakes only to be erased
// by the `mole down && mole up` typed at 10:02, after which nothing — not
// `mole status`, not the new daemon's log — mentioned the server that had been
// billing all night. This record outlives daemons on purpose and is cleared
// only when the debt is actually settled (see ClearLidDebt).
func LidDebtPath() string { return filepath.Join(MoleDir(), "lid-debt.json") }

// CustomRulesPath is the optional JSON file for user-defined routing rules.
func CustomRulesPath() string { return filepath.Join(MoleDir(), "custom-rules.json") }

// CeilingPath holds a manual Brutal-ceiling pin (`mole ceiling <n>`). Its
// presence overrides the supervisor's clock-driven profile; absent means auto.
// It is deliberately a file of its own rather than a field in state.json:
// state.json has a single writer (the daemon), and the ceiling CLI runs as a
// separate process, so sharing that file would race.
func CeilingPath() string { return filepath.Join(MoleDir(), "ceiling") }

// LegPath holds a manual transport-leg pin (`mole leg hy2|reality`). Absent
// means the supervisor chooses. A separate file from the ceiling pin for the
// same reason that one is separate from state.json — a different writer, and
// here also a different question: the ceiling asks how fast, the leg asks over
// what.
func LegPath() string { return filepath.Join(MoleDir(), "leg") }
