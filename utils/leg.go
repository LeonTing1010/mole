package utils

import (
	"fmt"
	"os"
	"strings"
)

// The manual transport-leg pin. Writing it makes the supervisor stop choosing
// between the VLESS+REALITY primary and the hysteria2 fallback ladder on
// evidence, and hold the leg named here; clearing it hands the choice back.
//
// It exists because the automatic failover answers exactly one question — "has
// the active transport stopped carrying while the other still dials?" — and
// there are cases it will never answer correctly on its own: measuring one leg
// against the other, or knowing something about tonight that the health
// signals do not. "Put me on UDP for the next ten minutes" is the whole ask,
// and this file is the only way to say it.
//
// Format is a bare word so the file is trivially inspectable — `cat ~/.mole/leg`
// answers "why is my tunnel on UDP?" without tooling.
const (
	// LegAuto is the unpinned state: REALITY is the primary and the supervisor
	// falls back to the hy2 ladder, and returns, on evidence.
	LegAuto = ""
	// LegHy2 pins the hysteria2/UDP ladder, bypassing the primary. It also
	// DISARMS the automatic return — see Supervisor.enterFallback. Pinning a
	// leg mole would have moved off is a deliberate act, usually to watch it.
	LegHy2 = "hy2"
	// LegReality pins the VLESS+REALITY/TCP primary, with the automatic
	// fallback disarmed.
	LegReality = "reality"
)

// ReadLeg returns the pinned leg, or LegAuto when unpinned. A malformed file is
// treated as unpinned rather than an error, for the same reason ReadCeiling
// does: a corrupt pin must degrade to automatic, never wedge the tunnel onto a
// leg nobody chose.
func ReadLeg() string {
	b, err := os.ReadFile(LegPath())
	if err != nil {
		return LegAuto
	}
	switch v := strings.ToLower(strings.TrimSpace(string(b))); v {
	case LegHy2, LegReality:
		return v
	default:
		return LegAuto
	}
}

// WriteLeg pins the transport leg.
func WriteLeg(leg string) error {
	switch leg {
	case LegHy2, LegReality:
		return os.WriteFile(LegPath(), []byte(leg+"\n"), 0644)
	default:
		return fmt.Errorf("unknown leg %q (want %q or %q)", leg, LegHy2, LegReality)
	}
}

// ClearLeg removes the pin, returning the choice to the supervisor. A missing
// file is success — clearing an unpinned leg is a no-op.
func ClearLeg() error {
	err := os.Remove(LegPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
