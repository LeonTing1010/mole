package core

import (
	"errors"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// The 2026-11-17 outage: a lid-close DELETE was carried out on the account but
// its answer never arrived, so the destroy was filed as failed. On reopening
// the lid the daemon asserted "still running" without asking anyone, and went
// on dialling an address with no host behind it for hours. These three tests
// pin the branch that now asks the provider first.
//
// The verify hook is the only new injection: everything else reuses the retry
// supervisor, including the $HOME redirect that keeps test state off the real
// daemon's files.
func newVerifySupervisor(t *testing.T, gone, ok bool) (*Supervisor, *int) {
	t.Helper()
	calls := 0
	s, _ := newRetrySupervisor(t, 0, errors.New("context deadline exceeded"))
	s.verifyServerGone = func() (bool, bool) {
		calls++
		return gone, ok
	}
	return s, &calls
}

// oweDebt puts the supervisor in the state the lid close left it in: a destroy
// attempted, failed, and still owed. lastLidAttempt is pushed into the past
// because the reconcile path rate-limits provider calls, and in the real
// sequence hours pass between the attempt and the reopen.
func oweDebt(t *testing.T, s *Supervisor) {
	t.Helper()
	if s.attemptLidDestroy("lid close", lidCloseBudget) {
		t.Fatal("the failing destroy reported success — no debt to test with")
	}
	if got := s.snapshot(); got.LidDestroy != utils.LidDestroyPending {
		t.Fatalf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyPending)
	}
	s.lastLidAttempt = time.Now().Add(-time.Hour)
}

func TestLidDebtVerifyLearnsTheDestroySucceeded(t *testing.T) {
	s, calls := newVerifySupervisor(t, true, true)
	oweDebt(t, s)

	s.reconcileLidDebt("wake retry")

	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyFired {
		t.Errorf("LidDestroy = %q, want %q — the account says the instance is gone, so the debt is settled",
			got.LidDestroy, utils.LidDestroyFired)
	}
	if !got.LidDestroyPendingSince.IsZero() {
		t.Errorf("PendingSince = %v, want zero once the debt is settled", got.LidDestroyPendingSince)
	}
	if *calls != 1 {
		t.Errorf("provider asked %d times, want 1", *calls)
	}
}

func TestLidDebtVerifyUnreadableAccountIsNotEvidenceOfLife(t *testing.T) {
	s, _ := newVerifySupervisor(t, false, false)
	oweDebt(t, s)

	s.reconcileLidDebt("wake retry")

	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyPending {
		t.Errorf("LidDestroy = %q, want %q — an account that cannot be read must leave the debt open, not cancel it",
			got.LidDestroy, utils.LidDestroyPending)
	}
	if got.LidDestroy == utils.LidDestroyCancelled {
		t.Error("an unreadable account was recorded as 'still running', which is the assertion this fix removes")
	}
}

func TestLidDebtVerifyServerStillThereCancels(t *testing.T) {
	s, calls := newVerifySupervisor(t, false, true)
	oweDebt(t, s)

	s.reconcileLidDebt("wake retry")

	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyCancelled {
		t.Errorf("LidDestroy = %q, want %q — the instance really is alive and the lid is open, so keep it",
			got.LidDestroy, utils.LidDestroyCancelled)
	}
	if *calls != 1 {
		t.Errorf("provider asked %d times, want 1", *calls)
	}
}

// Without a provider hook the old behaviour is unchanged, which is what the
// pre-existing retry tests rely on.
func TestLidDebtWithoutVerifyHookStillCancels(t *testing.T) {
	s, _ := newRetrySupervisor(t, 0, errors.New("context deadline exceeded"))
	oweDebt(t, s)

	s.reconcileLidDebt("wake retry")

	if got := s.snapshot(); got.LidDestroy != utils.LidDestroyCancelled {
		t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyCancelled)
	}
}
