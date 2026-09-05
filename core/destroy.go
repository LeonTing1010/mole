package core

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// ErrNothingToDestroy is what the destroy hook returns when there is nothing
// left to delete and it has *proven* it: no local entry, and the provider agrees
// no instance exists. Anything short of that proof is an ordinary failure and
// must stay one — the whole value of the debt is that it only ever names money
// actually being spent.
//
// It exists because "nothing to destroy" and "the destroy failed" arrive at this
// layer as the same non-nil error and mean opposite things. On 2026-08-15 a lid
// close destroyed the server at 12:45; a second lid close at 14:02 asked for the
// same name, got "no servers" back in 1ms, and filed it as a failed destroy. For
// the rest of the afternoon `mole status` reported a VPS billing unattended
// while the Vultr account held exactly one instance — the one created afterwards.
// A warning that cries wolf is worse than no warning: it is the same sentence
// the real orphan will use.
var ErrNothingToDestroy = errors.New("nothing to destroy")

// wakeRetryFloor is the minimum gap between destroy attempts made while a debt
// is outstanding.
//
// Wakes arrive in bursts, not on a cadence: on 2026-08-09 four dark wakes
// landed inside 90 seconds between 05:35 and 05:43. Without a floor each one
// would start another attempt down a path that answered nothing 8 seconds
// earlier, which is not a retry so much as the same failure four times. A
// minute is long enough that consecutive attempts are actually sampling
// different network conditions, and short enough that the debt is paid within
// one or two wakes of the link coming back.
//
// Measured in WALL time — see wallSince. It used to be measured with Go's
// monotonic clock, which on darwin stops while the machine is asleep, so "one
// minute" silently meant "one minute of accumulated awake time": on 2026-08-14
// the machine spent 6h21m asleep and accrued 4 minutes of it, and the first
// six dark wakes after the failed destroy were all inside the floor.
const wakeRetryFloor = time.Minute

// The two budgets a destroy gets, which are not the same problem.
//
// lidCloseBudget: the system has committed to sleep and is waiting for us,
// bounded at roughly 30 seconds, after which it sleeps mid-request. Twenty
// leaves margin and is the one window where patience is affordable.
//
// retryBudget: a dark wake ends on its own schedule — 2 seconds is typical on
// battery — and HoldAwake is a request macOS grants on AC and routinely
// declines on battery, which is the case that matters here. A long attempt does
// not survive that: it gets frozen mid-TLS and wakes up holding a dead socket.
// So a retry is deliberately one short bound attempt — if the link is there it
// finishes well inside it, and if it is not, the next tick costs nothing.
const (
	lidCloseBudget = 20 * time.Second
	retryBudget    = 6 * time.Second
)

// wallSince is time.Since with the monotonic reading stripped from both ends,
// so it measures the time that passed in the world rather than the time this
// process was awake for.
//
// Every duration in this file spans a sleep by definition, which is exactly
// where the two disagree: Go's monotonic clock does not advance while a Mac is
// asleep. The daemon that owed a destroy from 03:40 reported it at 10:01 as
// "the server billed 4m0s unused" — a true statement about how long the CPU had
// been running, and a useless one about the bill.
func wallSince(t time.Time) time.Duration {
	return time.Now().Round(0).Sub(t.Round(0))
}

// runSleepDestroy deletes the server when the lid closes.
//
// This is the whole policy, and it is deliberately not an idle timer. An idle
// timer cannot survive the thing it is waiting for: once the host sleeps the
// daemon is frozen, the count stops where it stood, and the server bills until
// morning. It also has to guess. Traffic is an unreliable stand-in for "this
// tunnel is wanted" in both directions — a machine at the login screen still
// chatters to push channels, and a peak-hour UDP blackout looks exactly like a
// quiet evening — and time-since-last-keypress has to pick a threshold that is
// always wrong for somebody.
//
// A closing lid is none of those things. It is a decision someone made with
// their hands, the system announces it, and it waits for an answer: the network
// is still up when the notification arrives, so the delete actually completes.
//
// Sleep in general is NOT that signal, which is what this used to act on. A
// laptop sleeps dozens of times a day without anyone leaving — dark-wake
// maintenance, sleep services, and the idle sleep that follows a blanked
// screen. Destroying on those deletes the server out from under someone who is
// still at their desk.
//
// No traffic check here. Whatever was downloading is about to be suspended
// anyway, so there is nothing to protect.
//
// The lid close is the first chance to destroy, not the only one. When it fails
// — and it fails exactly when it is least convenient, because a lid closes on
// the evening the path already went dark — the destroy becomes a debt that
// every subsequent tick and wake tries to pay; see reconcileLidDebt.
func (s *Supervisor) runSleepDestroy(ctx context.Context) {
	if s.lidDryRun {
		log.Printf("destroy-on-lid-close: DRY RUN armed — a lid close will be logged and nothing destroyed")
	} else {
		log.Printf("destroy-on-lid-close: armed — shutting the lid destroys the server")
	}

	go func() {
		defer recoverPanic("sleep-watch")
		if err := WatchPower(ctx, s.onLidClose, s.onPowerWake); err != nil {
			log.Printf("destroy-on-lid-close: unavailable: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		return
	case <-s.stopCh:
		return
	case <-s.lidDestroyed:
	}

	// Deliberately not torn down inside the callback. Everything the daemon
	// unwinds on the way out — DNS, the PF anchor, the UDP buffers — is host
	// state, and being frozen halfway through rewriting it would mean waking to
	// a machine pointed at a sing-box that no longer exists. Let the sleep win
	// this race; whenever the timer does fire, before sleep or on the next wake,
	// it restores the same thing.
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	case <-s.stopCh:
		return
	}
	s.Stop()
}

// attemptLidDestroy runs one destroy and records the outcome in state.json
// either way. Returns whether the server is gone.
func (s *Supervisor) attemptLidDestroy(why string, budget time.Duration) bool {
	s.lastLidAttempt = time.Now()
	// Retries live inside the management call, bounded by the budget the caller
	// passed for the window it is actually in. They belong there rather than
	// here because the failure this recovers from is a path failure, not a
	// Vultr failure: on 2026-08-05 the lid closed during a UDP blackout, the
	// DELETE rode the dead tunnel, and a single attempt returned EOF in the same
	// second it was made. The call now tries the physical interface first — a
	// ~0.9s round trip that does not touch the tunnel at all — so the budget
	// buys real alternatives instead of more waiting.
	started := time.Now()
	err := s.onIdleDestroy(budget)
	// Nothing to destroy is a settled state, not a failure: an earlier lid close
	// (or a `mole server destroy` typed in between) already did it, and the hook
	// has confirmed with the provider that no instance is left. Record it the
	// same way a successful destroy is recorded — including clearing any debt —
	// because the fact being recorded is identical: no server, no bill.
	if errors.Is(err, ErrNothingToDestroy) {
		log.Printf("destroy-on-lid-close: %s found no server to destroy — none exists on the account either, so nothing is billing", why)
		s.markLidSettled("no server to destroy at " + time.Now().Format("2006-01-02 15:04") + " — none was running")
		_ = utils.WriteState(s.snapshot())
		s.lidDestroyedOnce.Do(func() { close(s.lidDestroyed) })
		return true
	}
	if err != nil {
		s.recordLidDestroyPending(started, err)
		_, attempts, _ := s.lidDestroyPending()
		log.Printf("destroy-on-lid-close: %s failed after %s (attempt %d): %v — the server is STILL BILLING",
			why, time.Since(started).Round(time.Millisecond), attempts, err)
		_ = utils.WriteState(s.snapshot())
		return false
	}
	log.Printf("destroy-on-lid-close: %s destroyed the server in %s; bring it back with `mole server deploy` then `mole up`",
		why, time.Since(started).Round(time.Millisecond))
	// Flip the state marker before the daemon unwinds, so a `mole status`
	// during the shutdown window — or after the daemon was SIGKILL'd
	// mid-shutdown — reports "the server is gone" instead of a frozen
	// "tunnel dark". Cheap (one in-place rewrite) and safe on the sleep
	// thread; the machine is about to sleep, but this is a file rename, not
	// a network call.
	s.recordLidDestroyed(time.Now())
	_ = utils.WriteState(s.snapshot())
	s.lidDestroyedOnce.Do(func() { close(s.lidDestroyed) })
	return true
}

// onLidClose is the first chance to destroy: the system has committed to sleep
// and is waiting on us, so the network is still up.
func (s *Supervisor) onLidClose() {
	// The dry run exists because the clamshell test is the part that cannot be
	// proven by reading it: sleepwatch_darwin.go searches for a property whose
	// placement differs between models. Returning here leaves the daemon
	// running, so the lid can be closed and opened as many times as it takes to
	// trust the log line.
	if s.lidDryRun {
		log.Printf("destroy-on-lid-close: DRY RUN — lid close detected; would have destroyed the server now")
		return
	}
	// With a grace period the lid close is a note, not a destroy. The server
	// is kept until the first wake that finds the lid still shut past the
	// grace; a lid that reopens before then keeps it for good. See
	// reconcileLidDebt, which does the checking on every wake and tick.
	//
	// What this trades: a destroy at lid close has the network up and a 20s
	// window; one on a dark wake has 6s and a HoldAwake that battery may
	// decline — so a deferred destroy fails more often, and then the existing
	// debt machinery retries it on every later wake. A Mac that never
	// dark-wakes keeps the server until the lid opens, which at Vultr's rate
	// is a few cents a night. Against that: 2026-09-04 17:53, a lid shut for
	// two minutes, a destroy, a redeploy two minutes later, and a node that
	// booted with one leg. Vultr bills the hour either way.
	if s.lidGrace > 0 {
		if _, held := s.lidDeferred(); held {
			return
		}
		s.recordLidDeferred(time.Now())
		_ = utils.WriteState(s.snapshot())
		log.Printf("destroy-on-lid-close: the lid is shut — keeping the server for %s; it is destroyed on the first wake after that if the lid is still shut, and kept if the lid opens first", s.lidGrace)
		return
	}
	log.Printf("destroy-on-lid-close: the lid is shut — destroying the server")
	// Waits rather than skipping if a tick-driven retry is mid-flight: this is
	// the one call with a window the system is holding open for it, and a
	// retry's budget is short enough that the wait is bounded well inside it.
	s.lidMu.Lock()
	defer s.lidMu.Unlock()
	s.attemptLidDestroy("lid close", lidCloseBudget)
}

// lidGraceAction is what a wake or tick should do about a held destroy.
type lidGraceAction int

const (
	lidGraceWait    lidGraceAction = iota // still inside the grace, lid still shut
	lidGraceKeep                          // the lid opened: keep the server, re-arm
	lidGraceDestroy                       // grace elapsed with the lid still shut
)

// lidGraceDecision is the policy, kept free of the clock and the clamshell so
// it can be tested. closed follows lidClosed(): 1 shut, 0 open, -1 unknown;
// unknown counts as shut, for the reason reconcileLidDebt gives.
func lidGraceDecision(closed int, since, now time.Time, grace time.Duration) lidGraceAction {
	if closed == 0 {
		return lidGraceKeep
	}
	if now.Round(0).Sub(since.Round(0)) < grace {
		return lidGraceWait
	}
	return lidGraceDestroy
}

// onPowerWake settles a destroy at a wake notification. Kept as the fast path —
// a full wake is the earliest anyone learns the lid opened — but no longer the
// only one; see reconcileLidDebt.
func (s *Supervisor) onPowerWake() { s.reconcileLidDebt("wake retry") }

// reconcileLidDebt settles a destroy that the lid close could not finish, from
// whatever is ticking at the time. Safe to call as often as anything likes.
//
// It is called from the probe loop rather than only from the wake notification
// because the wake notification does not arrive. IOKit's
// kIOMessageSystemHasPoweredOn — the whole basis of the old retry path — is a
// full-wake event: on 2026-08-14 the machine took 28 dark wakes between 03:40
// and 10:01 with the daemon running through every one of them (its probe and
// keepalive loops are timestamped inside each), and the handler fired exactly
// once, at 10:01:50, on the full wake that came with the lid opening. The debt
// was six hours old by then. The supported way to hear a dark wake is
// IOPMConnection, which is not in the public SDK; a loop that already ticks
// through them is, and it costs one mutex read when nothing is owed.
//
// Costs nothing on the overwhelming majority of calls: no debt, immediate
// return. When there is one, the same clamshell read that gated the lid close
// now gates the retry, and it answers a genuinely different question in each
// direction. Still shut means nobody came back and the decision to destroy
// still stands. Open means they did, they want the tunnel, and deleting the
// server they are about to use would be obeying an instruction that has since
// been withdrawn.
//
// An unreadable clamshell (-1) retries. By the time there is a debt to settle a
// lid close has already been detected on this machine, so "cannot say" here is
// a failed read rather than a machine without a lid, and the standing decision
// is the better guess.
func (s *Supervisor) reconcileLidDebt(why string) {
	if s.onIdleDestroy == nil || s.lidDryRun {
		return
	}
	// A held destroy first: it is the common case with a grace period, and it
	// becomes a debt only if the attempt below fails.
	if heldSince, held := s.lidDeferred(); held {
		if !s.lidMu.TryLock() {
			return
		}
		defer s.lidMu.Unlock()
		switch lidGraceDecision(s.lidIsClosed(), heldSince, time.Now(), s.lidGrace) {
		case lidGraceKeep:
			log.Printf("destroy-on-lid-close: the lid opened %s after it shut — inside the grace period, so the server was kept and nothing was redeployed",
				wallSince(heldSince).Round(time.Minute))
			s.recordLidReopened()
			_ = utils.WriteState(s.snapshot())
		case lidGraceDestroy:
			if wallSince(s.lastLidAttempt) < wakeRetryFloor {
				return
			}
			log.Printf("destroy-on-lid-close: the lid has been shut for %s, past the %s grace — destroying the server",
				wallSince(heldSince).Round(time.Minute), s.lidGrace)
			if err := HoldAwake(); err != nil {
				log.Printf("destroy-on-lid-close: could not hold the machine awake (%v) — trying anyway", err)
			}
			defer ReleaseAwake()
			s.attemptLidDestroy(why, retryBudget)
		}
		return
	}
	since, attempts, owed := s.lidDestroyPending()
	if !owed {
		return
	}
	// Two callers now — the probe loop and the wake notification — and they can
	// land on the same debt at the same moment. Whoever got here first is
	// already doing the only useful thing.
	if !s.lidMu.TryLock() {
		return
	}
	defer s.lidMu.Unlock()
	if s.lidIsClosed() == 0 {
		// Not an error, and said plainly rather than apologetically: the server
		// is fine, it just cost money nobody got any use out of.
		unused := wallSince(since).Round(time.Minute)
		log.Printf("destroy-on-lid-close: the lid is open again — giving up on the destroy owed since %s "+
			"(%d attempts; the server billed %s unused and is still running)",
			since.Format("15:04"), attempts, unused)
		s.recordLidDestroyCancelled("lid opened before the destroy could complete; the server billed " +
			unused.String() + " unused and is still running")
		_ = utils.WriteState(s.snapshot())
		return
	}
	if wallSince(s.lastLidAttempt) < wakeRetryFloor {
		return
	}
	// Ask for the wake to be held open for the length of the attempt. It is a
	// request, not a guarantee — on battery macOS routinely declines — which is
	// why retryBudget is short enough to be worth trying unheld.
	if err := HoldAwake(); err != nil {
		log.Printf("destroy-on-lid-close: could not hold the machine awake to retry (%v) — trying anyway", err)
	}
	defer ReleaseAwake()
	s.attemptLidDestroy(why, retryBudget)
}
