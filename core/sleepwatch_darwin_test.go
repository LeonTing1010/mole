//go:build darwin

package core

import (
	"os/exec"
	"regexp"
	"testing"
)

// TestLidClosedMatchesRegistry checks the gate against the same value `ioreg`
// reports, because the failure this guards against is silent: a property read
// that returns nothing looks exactly like a lid that is open, and the only
// symptom is a server that never gets destroyed (or, before this gate existed,
// one destroyed on every idle sleep).
//
// The machine running the test almost certainly has its lid open, so this
// mostly pins the "open" answer. That is the half that matters — it is the one
// standing between an idle sleep and a deleted server.
func TestLidClosedMatchesRegistry(t *testing.T) {
	out, err := exec.Command("ioreg", "-r", "-k", "AppleClamshellState", "-d", "1", "-w", "0").Output()
	if err != nil {
		t.Skipf("ioreg unavailable: %v", err)
	}
	m := regexp.MustCompile(`"AppleClamshellState"\s*=\s*(Yes|No)`).FindSubmatch(out)
	if m == nil {
		// A desktop, or a model that does not publish it. -1 is the honest
		// answer there, and the callback treats it as "not a lid close".
		if got := lidClosed(); got == 1 {
			t.Fatalf("no clamshell state in the registry, yet lidClosed() = 1")
		}
		t.Skip("no AppleClamshellState published on this machine")
	}

	want := 0
	if string(m[1]) == "Yes" {
		want = 1
	}
	if got := lidClosed(); got != want {
		t.Fatalf("lidClosed() = %d, ioreg says AppleClamshellState=%s (want %d)", got, m[1], want)
	}
}
