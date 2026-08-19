package core

import (
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// state.json is what `mole status` reads, and the question it has to answer
// hours after the fact is "was the watcher armed when I shut the lid?" — so
// every startup must write one of the three answers, never leave the field at
// its zero value and let a reader infer.
func TestRecordLidDestroyWritesTheAnswerEitherWay(t *testing.T) {
	tests := []struct {
		name       string
		armed      bool
		dryRun     bool
		offReason  string
		want       utils.LidDestroyState
		wantReason string
	}{
		{
			name:  "armed",
			armed: true,
			want:  utils.LidDestroyArmed,
		},
		{
			name:   "dry run is reported as itself, not as armed",
			armed:  true,
			dryRun: true,
			want:   utils.LidDestroyDryRun,
		},
		{
			name:       "off carries the reason through to status",
			armed:      false,
			offReason:  "no Vultr key reached the daemon",
			want:       utils.LidDestroyOff,
			wantReason: "no Vultr key reached the daemon",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{
				LidCloseDryRun:      tc.dryRun,
				LidDestroyOffReason: tc.offReason,
			})
			s.recordLidDestroy(tc.armed)

			got := s.snapshot()
			if got.LidDestroy != tc.want {
				t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, tc.want)
			}
			if got.LidDestroy == "" {
				t.Error("left the field empty, which readers must treat as 'unknown daemon version'")
			}
			if tc.wantReason != "" && got.LidDestroyReason != tc.wantReason {
				t.Errorf("LidDestroyReason = %q, want %q", got.LidDestroyReason, tc.wantReason)
			}
		})
	}
}

// Once the destroy fires, the state must stop describing intent and say what
// happened — this is the answer `mole status` gives the operator who opens the
// laptop the morning after and finds the server gone. A fired marker left as
// "armed" reproduces the exact confusion from the 2026-08-07 incident: a frozen
// "tunnel dark" read through a server that no longer exists.
func TestRecordLidDestroyedMarksTheServerGone(t *testing.T) {
	s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{})
	s.recordLidDestroy(true) // arm first, as a real daemon would

	when := time.Date(2026, 8, 7, 4, 54, 37, 0, time.Local)
	s.recordLidDestroyed(when)

	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyFired {
		t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyFired)
	}
	if !strings.Contains(got.LidDestroyReason, "2026-08-07 04:54") {
		t.Errorf("LidDestroyReason = %q, want it to carry the destroy time", got.LidDestroyReason)
	}
}
