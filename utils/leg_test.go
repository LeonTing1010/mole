package utils

import (
	"os"
	"testing"
)

func TestReadLegUnpinnedByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := ReadLeg(); got != LegAuto {
		t.Fatalf("no pin file should read as auto, got %q", got)
	}
}

func TestWriteReadClearLeg(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, leg := range []string{LegHy2, LegReality, "both"} {
		if err := WriteLeg(leg); err != nil {
			t.Fatalf("WriteLeg(%q): %v", leg, err)
		}
		if got := ReadLeg(); got != leg {
			t.Fatalf("after WriteLeg(%q), ReadLeg = %q", leg, got)
		}
	}
	if err := ClearLeg(); err != nil {
		t.Fatalf("ClearLeg: %v", err)
	}
	if got := ReadLeg(); got != LegAuto {
		t.Fatalf("after ClearLeg, ReadLeg = %q, want auto", got)
	}
	// Clearing twice is what happens when someone runs `mole leg auto` on an
	// already-automatic tunnel. It must not be an error.
	if err := ClearLeg(); err != nil {
		t.Fatalf("ClearLeg on missing file: %v", err)
	}
}

func TestWriteLegRejectsUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := WriteLeg("quic"); err == nil {
		t.Fatal("WriteLeg should reject a leg that names no outbound")
	}
	if got := ReadLeg(); got != LegAuto {
		t.Fatalf("a rejected write must leave the pin untouched, got %q", got)
	}
}

// A pin file that cannot be understood must degrade to automatic. The failure
// it guards against is a wedged tunnel: a garbled file that read as "reality"
// would hold every user on the slow leg with no way to see why, and one that
// errored out would take the scheduler down with it.
func TestReadLegMalformedDegradesToAuto(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{"", "\n", "udp", "hy2 reality", "1", "  "} {
		if err := os.WriteFile(LegPath(), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if got := ReadLeg(); got != LegAuto {
			t.Fatalf("pin file %q read as %q, want auto", body, got)
		}
	}
}

// Whitespace and case are what a human editing the file by hand actually
// produces, and `cat ~/.mole/leg` being the documented way to answer "why is my
// tunnel on TCP?" means editing it by hand is expected.
func TestReadLegToleratesHumanEditing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{"reality\n", "  reality  \n", "REALITY", "Reality\n\n"} {
		if err := os.WriteFile(LegPath(), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if got := ReadLeg(); got != LegReality {
			t.Fatalf("pin file %q read as %q, want %q", body, got, LegReality)
		}
	}
}
