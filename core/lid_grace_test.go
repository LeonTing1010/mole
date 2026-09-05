package core

import (
	"testing"
	"time"
)

// The grace policy: reopen keeps, shut-and-early waits, shut-and-late
// destroys, and an unreadable clamshell counts as shut.
func TestLidGraceDecision(t *testing.T) {
	grace := 30 * time.Minute
	since := time.Date(2026, 9, 4, 17, 53, 0, 0, time.Local)
	cases := []struct {
		name   string
		closed int
		after  time.Duration
		want   lidGraceAction
	}{
		{"reopened at once", 0, time.Minute, lidGraceKeep},
		{"reopened late", 0, 2 * time.Hour, lidGraceKeep},
		{"shut, 2 minutes in", 1, 2 * time.Minute, lidGraceWait},
		{"shut, one second short", 1, grace - time.Second, lidGraceWait},
		{"shut, grace elapsed", 1, grace, lidGraceDestroy},
		{"shut, morning", 1, 8 * time.Hour, lidGraceDestroy},
		{"unknown clamshell, early", -1, time.Minute, lidGraceWait},
		{"unknown clamshell, late", -1, time.Hour, lidGraceDestroy},
	}
	for _, c := range cases {
		if got := lidGraceDecision(c.closed, since, since.Add(c.after), grace); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
