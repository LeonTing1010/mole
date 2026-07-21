package utils

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The manual ceiling pin. Writing it makes the supervisor's bandwidth scheduler
// stop following the clock and hold the pinned rung; clearing it hands control
// back. The value is the Brutal DOWN ceiling in Mbps, which is also what names
// the selector member the scheduler switches to (core.BandwidthRungTag).
//
// Format is a bare integer so the file is trivially inspectable and editable —
// `cat ~/.mole/ceiling` answers "why is my ceiling stuck at 8?" without tooling.

// ReadCeiling returns the pinned down-Mbps ceiling, or 0 when unpinned (auto).
// A malformed or non-positive file is treated as unpinned rather than an error:
// a corrupt pin should degrade to automatic scheduling, never wedge the tunnel.
func ReadCeiling() int {
	b, err := os.ReadFile(CeilingPath())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// WriteCeiling pins the ceiling to down Mbps.
func WriteCeiling(down int) error {
	if down <= 0 {
		return fmt.Errorf("ceiling must be positive, got %d", down)
	}
	return os.WriteFile(CeilingPath(), []byte(strconv.Itoa(down)+"\n"), 0644)
}

// ClearCeiling removes the pin, returning the scheduler to clock control.
// A missing file is success — clearing an unpinned ceiling is a no-op.
func ClearCeiling() error {
	err := os.Remove(CeilingPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
