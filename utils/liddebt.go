package utils

import (
	"encoding/json"
	"os"
	"time"
)

// LidDebt is the record of a destroy that fired when the lid shut and never
// completed: which server is still running, since when, and how many attempts
// have been spent on it.
//
// It duplicates fields that state.json also carries while a daemon is alive,
// and that duplication is the point. state.json answers "what is the running
// daemon doing"; this answers "what does someone still owe money for", which is
// a question that outlives the daemon, the reboot, and the `mole down` typed in
// the morning by someone who has not read the warning yet.
type LidDebt struct {
	// Server is the local server name (servers.json label) whose instance is
	// still billing. Used to clear the debt when that server is finally
	// destroyed, so a stale record cannot nag forever.
	Server string `json:"server,omitempty"`
	// Since is when the lid shut and the first attempt failed — wall clock, so
	// it stays meaningful across the sleep that follows. Never restamped by a
	// retry: it measures the debt, not the last attempt.
	Since time.Time `json:"since"`
	// Attempts counts every destroy tried since Since, successful or not.
	Attempts int `json:"attempts"`
	// Reason is the last failure in the operator's terms.
	Reason string `json:"reason,omitempty"`
	// State is LidDestroyPending while something is still trying, or
	// LidDestroyCancelled once the lid opened and the destroy was abandoned.
	State LidDestroyState `json:"state"`
}

// WriteLidDebt persists the debt, replacing any previous record.
func WriteLidDebt(d *LidDebt) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	path := LidDebtPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ChownToMoleDirOwner(path)
	return nil
}

// ReadLidDebt returns the outstanding debt, or nil when nothing is owed.
// A malformed file reads as "nothing owed" rather than an error: every caller
// is on its way somewhere else, and a debt nobody can parse is one nobody can
// act on either.
func ReadLidDebt() *LidDebt {
	data, err := os.ReadFile(LidDebtPath())
	if err != nil {
		return nil
	}
	var d LidDebt
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	if d.Since.IsZero() {
		return nil
	}
	return &d
}

// ClearLidDebt forgets the debt. Call it only when the server it names is
// actually gone — a destroy that succeeded, or a `mole server destroy` that
// removed the same entry. Clearing it for any other reason silently drops the
// only record that a VPS is billing unattended.
func ClearLidDebt() error {
	err := os.Remove(LidDebtPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
