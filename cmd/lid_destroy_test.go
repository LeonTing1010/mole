package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The guard exists because `sudo mole up` is indistinguishable from a working
// invocation right up until the moment it fails to destroy anything, hours
// later. It must reject exactly that case and nothing else — in particular not
// the sudo re-exec `mole up` performs on itself, which is the normal path and
// arrives with the key already stashed.
func TestSudoWouldDisarmLidDestroy(t *testing.T) {
	tests := []struct {
		name         string
		euid         int
		sudoUser     string
		keyAvailable bool
		optedOut     bool
		dryRun       bool
		want         bool
	}{
		{
			name: "operator ran sudo mole up with no stashed key",
			euid: 0, sudoUser: "leo", keyAvailable: false,
			want: true,
		},
		{
			name: "the re-exec mole performs on itself: key was stashed before sudo",
			euid: 0, sudoUser: "leo", keyAvailable: true,
			want: false,
		},
		{
			name: "unprivileged first pass, key comes from the environment",
			euid: 501, sudoUser: "", keyAvailable: true,
			want: false,
		},
		{
			name: "unprivileged first pass with no key at all — warn, do not refuse",
			euid: 501, sudoUser: "", keyAvailable: false,
			want: false,
		},
		{
			name: "explicit opt-out under sudo is a decision, not a mistake",
			euid: 0, sudoUser: "leo", keyAvailable: false, optedOut: true,
			want: false,
		},
		{
			name: "dry run needs no key, so sudo cannot disarm it",
			euid: 0, sudoUser: "leo", keyAvailable: false, dryRun: true,
			want: false,
		},
		{
			name: "root login shell (no SUDO_USER) is not an operator's sudo",
			euid: 0, sudoUser: "", keyAvailable: false,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sudoWouldDisarmLidDestroy(tc.euid, tc.sudoUser, tc.keyAvailable, tc.optedOut, tc.dryRun)
			if got != tc.want {
				t.Errorf("sudoWouldDisarmLidDestroy(%d, %q, key=%v, opt=%v, dry=%v) = %v, want %v",
					tc.euid, tc.sudoUser, tc.keyAvailable, tc.optedOut, tc.dryRun, got, tc.want)
			}
		})
	}
}

// Every disarmed branch must carry a reason. A plan that returns armed=false
// with an empty explanation reproduces the original bug in a new place: the
// operator learns the watcher is off but not which of three unrelated causes
// put it there.
func TestLidDestroyPlanAlwaysExplainsItself(t *testing.T) {
	// The flags are package-level (cobra binds them there); restore them so
	// ordering between tests cannot matter.
	defer func(a, b bool) { noDestroyOnLidClose, lidCloseDryRun = a, b }(noDestroyOnLidClose, lidCloseDryRun)

	t.Run("opt-out", func(t *testing.T) {
		noDestroyOnLidClose, lidCloseDryRun = true, false
		armed, dryRun, reason := lidDestroyPlan()
		if armed || dryRun {
			t.Fatalf("armed=%v dryRun=%v, want both false", armed, dryRun)
		}
		if reason == "" {
			t.Fatal("opting out produced no reason to show the operator")
		}
	})

	t.Run("dry run arms the watcher without a key", func(t *testing.T) {
		noDestroyOnLidClose, lidCloseDryRun = false, true
		// Deliberately no key in the environment: proving the clamshell
		// detection works must not require credentials that can destroy.
		t.Setenv("VULTR_API_KEY", "")
		withoutStashedKey(t)
		armed, dryRun, _ := lidDestroyPlan()
		if !armed || !dryRun {
			t.Fatalf("armed=%v dryRun=%v, want armed dry run", armed, dryRun)
		}
	})

	t.Run("no key", func(t *testing.T) {
		noDestroyOnLidClose, lidCloseDryRun = false, false
		t.Setenv("VULTR_API_KEY", "")
		withoutStashedKey(t)
		armed, _, reason := lidDestroyPlan()
		if armed {
			t.Fatal("armed with no key available — the destroy could not authenticate")
		}
		if reason == "" {
			t.Fatal("a missing key produced no reason to show the operator")
		}
	})

	t.Run("key present", func(t *testing.T) {
		noDestroyOnLidClose, lidCloseDryRun = false, false
		t.Setenv("VULTR_API_KEY", "test-key")
		armed, dryRun, reason := lidDestroyPlan()
		if !armed || dryRun {
			t.Fatalf("armed=%v dryRun=%v, want a live arm", armed, dryRun)
		}
		if reason != "" {
			t.Errorf("armed but reported %q as a reason it is not", reason)
		}
	})
}

// withoutStashedKey points HOME at an empty directory so the on-disk fallback
// in vultrKey() cannot see a real stash left by a running daemon.
func withoutStashedKey(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if _, err := os.Stat(filepath.Join(dir, ".mole", "vultr_key")); err == nil {
		t.Fatal("temp home already has a stashed key")
	}
}
