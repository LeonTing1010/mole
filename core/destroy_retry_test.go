package core

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// newRetrySupervisor builds a supervisor whose destroy is a counter and whose
// clamshell answer is whatever the test says it is.
//
// Both injections are necessary rather than convenient. The machine running
// these tests has its lid open — that is what "running tests" means — so the
// branch that matters most, a wake with the lid still shut, is unreachable
// without a fake. And the state writes go through utils.MoleDir, which is
// derived from $HOME: without redirecting it, a test run would overwrite the
// live daemon's state.json on the developer's own laptop.
func newRetrySupervisor(t *testing.T, lid int, results ...error) (*Supervisor, *int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	calls := 0
	s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{
		DestroyOnLidClose: true,
		OnIdleDestroy: func(time.Duration) error {
			calls++
			if calls <= len(results) {
				return results[calls-1]
			}
			return nil
		},
	})
	s.lidClosedFn = func() int { return lid }
	s.recordLidDestroy(true)
	return s, &calls
}

// The failure this whole mechanism exists for: the lid shut, the destroy fired,
// the network was dead, and before this the daemon simply moved on. state.json
// kept saying "armed" while the server billed until morning, so neither `mole
// status` nor the next `mole up` had any way to know a server was running that
// nobody wanted.
func TestFailedLidDestroyBecomesADebt(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, errors.New("context deadline exceeded"))

	if s.attemptLidDestroy("lid close", lidCloseBudget) {
		t.Fatal("attemptLidDestroy reported success on a failing destroy")
	}
	if *calls != 1 {
		t.Fatalf("destroy called %d times, want 1", *calls)
	}

	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyPending {
		t.Errorf("LidDestroy = %q, want %q — a failed destroy that still reads 'armed' is the original bug",
			got.LidDestroy, utils.LidDestroyPending)
	}
	if got.LidDestroyAttempts != 1 {
		t.Errorf("LidDestroyAttempts = %d, want 1", got.LidDestroyAttempts)
	}
	if got.LidDestroyPendingSince.IsZero() {
		t.Error("PendingSince is zero; nothing can say how long the server has been billing")
	}
	if got.LidDestroyReason == "" {
		t.Error("the failure cause was dropped")
	}
}

// The wake is the second chance, and on the night this was written there were
// twenty of them between the failed destroy and the morning. Each one that
// finds the lid still shut means nobody came back, so the decision made at the
// lid close still stands.
func TestWakeWithTheLidStillShutRetriesAndSettlesTheDebt(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, errors.New("no route to host"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	// Backdate the last attempt past the floor; a real wake arrives minutes
	// later, not microseconds.
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)

	s.onPowerWake()

	if *calls != 2 {
		t.Fatalf("destroy called %d times, want 2 (the lid close and one wake retry)", *calls)
	}
	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyFired {
		t.Errorf("LidDestroy = %q, want %q — the retry succeeded and the server is gone",
			got.LidDestroy, utils.LidDestroyFired)
	}
	if got.LidDestroyPendingSince != (time.Time{}) || got.LidDestroyAttempts != 0 {
		t.Errorf("the paid debt was left behind: since=%v attempts=%d",
			got.LidDestroyPendingSince, got.LidDestroyAttempts)
	}
	select {
	case <-s.lidDestroyed:
	default:
		t.Error("a successful wake retry did not signal the teardown path, so the daemon would keep running against a deleted server")
	}
}

// The opposite reading of the same property, and the one that keeps this from
// being dangerous. An open lid means their owner came back and wants the
// tunnel; deleting the server now would carry out an instruction that has been
// withdrawn, and cost a redeploy on top of the hours already paid for.
func TestWakeWithTheLidOpenAbandonsTheDestroy(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, errors.New("i/o timeout"))
	s.attemptLidDestroy("lid close", lidCloseBudget)
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)

	s.lidClosedFn = func() int { return 0 } // they opened it
	s.onPowerWake()

	if *calls != 1 {
		t.Fatalf("destroy called %d times, want 1 — the wake must not delete a server that is about to be used", *calls)
	}
	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyCancelled {
		t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyCancelled)
	}
	if got.LidDestroyPendingSince.IsZero() {
		t.Error("PendingSince was cleared; the record of how long the server billed unused is the only thing left to learn here")
	}

	// And it stays abandoned — a later wake must not resurrect it.
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
	s.onPowerWake()
	if *calls != 1 {
		t.Errorf("destroy called %d times after cancellation, want 1", *calls)
	}
}

// An unreadable clamshell is not an open one. By the time a debt exists a lid
// close has already been detected on this machine, so -1 is a failed read, and
// the standing decision is the better guess.
func TestWakeWithAnUnreadableClamshellStillRetries(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, errors.New("i/o timeout"))
	s.attemptLidDestroy("lid close", lidCloseBudget)
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)

	s.lidClosedFn = func() int { return -1 }
	s.onPowerWake()

	if *calls != 2 {
		t.Errorf("destroy called %d times, want 2 — an unreadable lid must not silently cancel the debt", *calls)
	}
}

// Wakes arrive in bursts: four landed inside 90 seconds on 2026-08-09 between
// 05:35 and 05:43. Without a floor each would start another 20-second attempt
// down a path that answered nothing seconds earlier — the same failure four
// times, not four retries.
func TestWakeStormMakesOneAttempt(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1,
		errors.New("i/o timeout"), errors.New("i/o timeout"),
		errors.New("i/o timeout"), errors.New("i/o timeout"), errors.New("i/o timeout"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	before := *calls

	for i := 0; i < 4; i++ {
		s.onPowerWake()
	}
	if *calls != before {
		t.Errorf("destroy called %d times during a wake storm, want %d — the %s floor did not hold",
			*calls, before, wakeRetryFloor)
	}

	// Past the floor, the next wake is allowed through.
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
	s.onPowerWake()
	if *calls != before+1 {
		t.Errorf("destroy called %d times after the floor elapsed, want %d", *calls, before+1)
	}
	if got := s.snapshot(); got.LidDestroyAttempts != before+1 {
		t.Errorf("LidDestroyAttempts = %d, want %d", got.LidDestroyAttempts, before+1)
	}
}

// Nothing is owed on an ordinary wake, which is nearly all of them. The handler
// must not touch the clamshell, the network, or the state on those.
func TestWakeWithNoDebtDoesNothing(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1)
	read := 0
	s.lidClosedFn = func() int { read++; return 1 }

	s.onPowerWake()

	if *calls != 0 {
		t.Errorf("destroy called %d times with no debt outstanding, want 0", *calls)
	}
	if read != 0 {
		t.Errorf("read the clamshell %d times with no debt outstanding, want 0", read)
	}
	if got := s.snapshot(); got.LidDestroy != utils.LidDestroyArmed {
		t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyArmed)
	}
}

// A dry run proves the clamshell detection without deleting anything, so it
// must stay inert on both halves of the mechanism — including the wake.
func TestDryRunNeverDestroysOnEitherPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{
		DestroyOnLidClose: true,
		LidCloseDryRun:    true,
		OnIdleDestroy:     func(time.Duration) error { calls++; return nil },
	})
	s.lidClosedFn = func() int { return 1 }
	s.recordLidDestroy(true)

	s.onLidClose()
	s.onPowerWake()
	s.reconcileLidDebt("tick retry")

	if calls != 0 {
		t.Errorf("destroy called %d times under --lid-close-dry-run, want 0", calls)
	}
}

// The bug this file's mechanism was supposed to prevent, and did not.
//
// On 2026-08-14 the destroy failed at 03:40 and the debt stood until 10:01. The
// Mac dark-woke 28 times in between with the daemon running through every one
// of them — its probe loop is timestamped inside each — but the retry hung off
// IOKit's kIOMessageSystemHasPoweredOn, which is a FULL-wake message. The
// handler fired once, six hours late, when the lid opened.
//
// So the retry must be reachable from something that ticks, with no wake
// notification anywhere in the call path. That is what this test drives.
func TestTickRetryPaysTheDebtWithNoWakeNotification(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, errors.New("context deadline exceeded"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	if *calls != 1 {
		t.Fatalf("destroy called %d times at the lid close, want 1", *calls)
	}

	// The next tick inside a dark wake, a minute of wall time later.
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
	s.reconcileLidDebt("tick retry")

	if *calls != 2 {
		t.Fatalf("destroy called %d times, want 2 — the tick path did not retry", *calls)
	}
	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyFired {
		t.Errorf("LidDestroy = %q, want %q — the retry succeeded but the debt was not settled",
			got.LidDestroy, utils.LidDestroyFired)
	}
	if utils.ReadLidDebt() != nil {
		t.Error("the debt record survived a successful destroy; `mole status` would nag about a server that is gone")
	}
}

// A destroy at a closing lid gets a held-open ~30s window; a retry gets whatever
// is left of a 2-second dark wake and nothing can hold it open (HoldAwake is a
// request macOS declines on battery). Spending the lid-close budget inside a
// dark wake just means being frozen mid-request.
func TestRetriesGetTheShortBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var budgets []time.Duration
	s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{
		DestroyOnLidClose: true,
		OnIdleDestroy: func(b time.Duration) error {
			budgets = append(budgets, b)
			return errors.New("i/o timeout")
		},
	})
	s.lidClosedFn = func() int { return 1 }
	s.recordLidDestroy(true)

	s.onLidClose()
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
	s.reconcileLidDebt("tick retry")

	if len(budgets) != 2 {
		t.Fatalf("got %d attempts, want 2", len(budgets))
	}
	if budgets[0] != lidCloseBudget {
		t.Errorf("lid close ran with budget %s, want %s", budgets[0], lidCloseBudget)
	}
	if budgets[1] != retryBudget {
		t.Errorf("retry ran with budget %s, want %s", budgets[1], retryBudget)
	}
	if retryBudget >= lidCloseBudget {
		t.Errorf("retryBudget %s is not shorter than lidCloseBudget %s", retryBudget, lidCloseBudget)
	}
}

// The debt has to outlive the daemon that incurred it. `mole down` deletes
// state.json, so the version of this record that lived only there was erased by
// the `mole down && mole up` typed the morning after — and with it the only
// thing that would have told anyone a VPS had been billing since 03:40.
func TestTheDebtSurvivesTheStateFileBeingDeleted(t *testing.T) {
	s, _ := newRetrySupervisor(t, 1, errors.New("context deadline exceeded"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	if err := utils.RemoveState(); err != nil {
		t.Fatalf("remove state: %v", err)
	}

	debt := utils.ReadLidDebt()
	if debt == nil {
		t.Fatal("the debt did not survive `mole down` deleting state.json")
	}
	if debt.Server != "test-server" {
		t.Errorf("debt names server %q, want the one still billing", debt.Server)
	}
	if debt.Attempts != 1 {
		t.Errorf("debt attempts = %d, want 1", debt.Attempts)
	}
	if debt.State != utils.LidDestroyPending {
		t.Errorf("debt state = %q, want %q", debt.State, utils.LidDestroyPending)
	}
	if debt.Since.IsZero() {
		t.Error("debt has no timestamp; nothing can say how long the server has been billing")
	}
}

// The retry has two callers now — the probe loop and the wake notification —
// and on a full wake both fire within milliseconds of each other. Two DELETEs
// in flight for the same instance is at best a wasted 404 and at worst two
// answers about one server; one attempt at a time is the invariant.
func TestASecondCallerDoesNotStartASecondDestroy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	s := NewSupervisor("/nonexistent/config.json", "test-server", SupervisorOpts{
		DestroyOnLidClose: true,
		OnIdleDestroy: func(time.Duration) error {
			// The first attempt returns; every later one parks so a second
			// caller is guaranteed to arrive mid-flight rather than by luck.
			if atomic.AddInt32(&calls, 1) > 1 {
				close(entered)
				<-release
			}
			return errors.New("i/o timeout")
		},
	})
	s.lidClosedFn = func() int { return 1 }
	s.recordLidDestroy(true)

	s.onLidClose() // incurs the debt
	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.onPowerWake()
	}()
	<-entered

	s.reconcileLidDebt("tick retry")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("destroy called %d times, want 2 — a tick started a second destroy on top of one in flight", got)
	}

	close(release)
	<-done
}

// Giving up because the lid opened is still a debt: the server is running and
// nobody destroyed it. The record has to say so, and has to survive.
func TestCancellingKeepsTheDebtRecord(t *testing.T) {
	s, _ := newRetrySupervisor(t, 1, errors.New("i/o timeout"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	s.lidClosedFn = func() int { return 0 } // they came back
	s.reconcileLidDebt("tick retry")

	debt := utils.ReadLidDebt()
	if debt == nil {
		t.Fatal("no debt recorded after the destroy was abandoned")
	}
	if debt.State != utils.LidDestroyCancelled {
		t.Errorf("debt state = %q, want %q", debt.State, utils.LidDestroyCancelled)
	}
}

// PendingSince marks when the debt was incurred, not when it was last chased.
// Resetting it per attempt would erase the only number that says how long the
// server has been billing for nobody.
func TestRepeatedFailuresKeepTheOriginalTimestamp(t *testing.T) {
	s, _ := newRetrySupervisor(t, 1,
		errors.New("first"), errors.New("second"), errors.New("third"))

	s.attemptLidDestroy("lid close", lidCloseBudget)
	first := s.snapshot().LidDestroyPendingSince

	for i := 0; i < 2; i++ {
		s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
		s.onPowerWake()
	}

	got := s.snapshot()
	if !got.LidDestroyPendingSince.Equal(first) {
		t.Errorf("PendingSince moved from %v to %v across retries", first, got.LidDestroyPendingSince)
	}
	if got.LidDestroyAttempts != 3 {
		t.Errorf("LidDestroyAttempts = %d, want 3", got.LidDestroyAttempts)
	}
	if got.LidDestroyReason != "third" {
		t.Errorf("LidDestroyReason = %q, want the most recent failure", got.LidDestroyReason)
	}
}

// The phantom debt of 2026-08-15. The lid shut at 12:45 and the destroy
// succeeded; the lid shut again at 14:02 with nothing left to delete, the hook
// returned in 1ms, and that was filed as a failed destroy. `mole status` then
// spent the afternoon reporting a VPS billing unattended — 18 minutes and
// counting — while the account held exactly one instance, created after the fact.
//
// A destroy hook that reports nothing to destroy has, by the contract on
// ErrNothingToDestroy, already confirmed the account is empty. Nothing is
// billing, so nothing is owed, and no record may claim otherwise.
func TestNothingToDestroyIsNotADebt(t *testing.T) {
	s, calls := newRetrySupervisor(t, 1, ErrNothingToDestroy)

	if !s.attemptLidDestroy("lid close", lidCloseBudget) {
		t.Fatal("attemptLidDestroy reported failure when there was simply nothing left to destroy")
	}
	if *calls != 1 {
		t.Fatalf("destroy called %d times, want 1", *calls)
	}

	if debt := utils.ReadLidDebt(); debt != nil {
		t.Errorf("wrote a debt for a server that does not exist (%q, %s) — `mole status` would bill the user for nothing",
			debt.Server, debt.Reason)
	}
	got := s.snapshot()
	if got.LidDestroy != utils.LidDestroyFired {
		t.Errorf("LidDestroy = %q, want %q", got.LidDestroy, utils.LidDestroyFired)
	}
	if !got.LidDestroyPendingSince.IsZero() || got.LidDestroyAttempts != 0 {
		t.Errorf("left a pending destroy behind: since=%v attempts=%d", got.LidDestroyPendingSince, got.LidDestroyAttempts)
	}
}

// An outstanding debt must survive a later lid close that finds nothing local —
// but only because the hook proves the account is empty, which also proves the
// old debt was already paid. The dangerous inverse is covered in cmd: a hook
// that cannot reach the account returns an ordinary error and the debt stands.
func TestNothingToDestroySettlesAnOutstandingDebt(t *testing.T) {
	s, _ := newRetrySupervisor(t, 1, errors.New("context deadline exceeded"), ErrNothingToDestroy)

	s.attemptLidDestroy("lid close", lidCloseBudget)
	if utils.ReadLidDebt() == nil {
		t.Fatal("a failed destroy left no debt")
	}

	s.lastLidAttempt = time.Now().Add(-2 * wakeRetryFloor)
	s.reconcileLidDebt("tick retry")

	if debt := utils.ReadLidDebt(); debt != nil {
		t.Errorf("the debt outlived proof that no instance exists: %+v", debt)
	}
}
