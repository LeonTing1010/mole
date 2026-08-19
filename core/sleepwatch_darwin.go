//go:build darwin

package core

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <CoreFoundation/CoreFoundation.h>

extern void moleWillSleep(void);
extern void moleDidWake(void);

static io_connect_t          gRootPort;
static IONotificationPortRef gNotifyPort;
static io_object_t           gNotifier;
static CFRunLoopRef          gRunLoop;
static IOPMAssertionID       gAwakeAssertion;

// moleHoldAwake asks power management to keep the system out of sleep for the
// length of a destroy, and moleReleaseAwake lets it go again.
//
// A request, not a guarantee, and the difference matters. A dark wake is short
// — 2 to 19 seconds on the nights this was measured — and ends whenever
// whatever caused it is finished, with no obligation to wait for us.
//
// kIOPMAssertNetworkClientActive is the assertion that covers this: the header
// describes it as keeping the system awake "in dark or full wake, as long as
// the system is on AC power", and on battery says plainly that OS X may not
// honor it. So a laptop asleep in a bag — the exact case this feature exists
// for — is the case where the hold does nothing, and the caller must be sized
// to survive that (see retryBudget).
//
// It replaced kIOPMAssertionTypePreventSystemSleep, which was not a weaker
// version of this but an inert one: deprecated in 10.9 and, per IOPMLib.h,
// "not supported in any OS X releases". IOPMAssertionCreateWithName still
// returns kIOReturnSuccess for it and `pmset -g assertions` still lists it
// under this process — while the system-wide PreventSystemSleep counter stays
// at 0. Measured on macOS 26 on 2026-08-14, which is how a comment claiming
// this was "what makes a retry on a dark wake possible at all" survived a year
// of never having held anything open.
//
// No root needed, for either type.
static int moleHoldAwake(void) {
    if (gAwakeAssertion != 0) {
        return 0;
    }
    IOReturn r = IOPMAssertionCreateWithName(kIOPMAssertNetworkClientActive,
                                             kIOPMAssertionLevelOn,
                                             CFSTR("mole: completing a pending server destroy"),
                                             &gAwakeAssertion);
    if (r != kIOReturnSuccess) {
        gAwakeAssertion = 0;
        return -1;
    }
    return 0;
}

static void moleReleaseAwake(void) {
    if (gAwakeAssertion != 0) {
        IOPMAssertionRelease(gAwakeAssertion);
        gAwakeAssertion = 0;
    }
}

// moleLidIsClosed answers whether the sleep now underway is a closing lid.
// Returns 1 for yes, 0 for no, -1 when the machine cannot say (a desktop with
// no clamshell at all, or a registry read that failed).
//
// The distinction matters because kIOMessageSystemWillSleep is not the rare
// event it reads as. A laptop on battery re-sleeps out of dark wake constantly
// — 'Maintenance Sleep', 'Sleep Service Back to Sleep', and the plain 'Idle
// Sleep' that follows the display going dark are all the same message to a
// power-management client. Acting on all of them destroys the server minutes
// after the screen blanks, while its owner is still sitting in front of it.
//
// AppleClamshellState is the direct answer and is already updated by the time
// this message arrives. "Sleep Reason" is the corroborating one — root domain
// publishes it just before notifying clients, and it reads "Clamshell Sleep"
// for exactly this case.
static int moleLidIsClosed(void) {
    io_service_t root = IOServiceGetMatchingService(kIOMainPortDefault,
                                                   IOServiceMatching("IOPMrootDomain"));
    if (root == MACH_PORT_NULL) {
        return -1;
    }
    int closed = -1;

    // Recursive: on some models the property is published by a child of root
    // domain rather than root domain itself.
    CFTypeRef state = IORegistryEntrySearchCFProperty(root, kIOServicePlane,
                                                      CFSTR("AppleClamshellState"),
                                                      kCFAllocatorDefault,
                                                      kIORegistryIterateRecursively);
    if (state != NULL) {
        if (CFGetTypeID(state) == CFBooleanGetTypeID()) {
            closed = CFBooleanGetValue((CFBooleanRef)state) ? 1 : 0;
        }
        CFRelease(state);
    }

    if (closed != 1) {
        CFTypeRef reason = IORegistryEntryCreateCFProperty(root, CFSTR("Sleep Reason"),
                                                          kCFAllocatorDefault, 0);
        if (reason != NULL) {
            if (CFGetTypeID(reason) == CFStringGetTypeID() &&
                CFStringFind((CFStringRef)reason, CFSTR("Clamshell"),
                             kCFCompareCaseInsensitive).location != kCFNotFound) {
                closed = 1;
            }
            CFRelease(reason);
        }
    }

    IOObjectRelease(root);
    return closed;
}

static void moleSleepCallback(void *refCon, io_service_t service,
                              natural_t messageType, void *messageArgument) {
    switch (messageType) {
    case kIOMessageCanSystemSleep:
        // Never veto. This message asks whether sleep may proceed, and the
        // answer is always yes: the lid is shut, and a laptop that refuses to
        // sleep in a bag is a far worse outcome than any billing we could save
        // by stalling here.
        IOAllowPowerChange(gRootPort, (long)messageArgument);
        break;
    case kIOMessageSystemWillSleep:
        // Sleep is now committed but has not happened yet, and the system waits
        // for IOAllowPowerChange before proceeding — bounded at roughly 30
        // seconds, after which it sleeps regardless. That window is the whole
        // point: the network is still up, so the destroy can actually complete.
        //
        // Anything that is not a closing lid is left alone. A destroy that
        // fires on idle or maintenance sleep costs a redeploy and two minutes
        // of boot script for someone who never went anywhere; not firing costs
        // a few cents. Only act when the machine says the lid is shut.
        if (moleLidIsClosed() == 1) {
            moleWillSleep();
        }
        IOAllowPowerChange(gRootPort, (long)messageArgument);
        break;
    case kIOMessageSystemHasPoweredOn:
        // FULL wakes only, whatever the name suggests. The dark wakes a laptop
        // takes all night do not deliver this message: on 2026-08-14 there were
        // 28 of them between the lid closing and the lid opening, the daemon
        // ran through every one, and this callback fired exactly once — at the
        // end, on the full wake. (Hearing dark wake needs IOPMConnection, which
        // is not in the public SDK.)
        //
        // So this is the fast path for "someone came back", not the retry
        // engine it was written as. What settles a debt across a night is
        // Supervisor.reconcileLidDebt, called from a loop that does tick inside
        // dark wakes.
        moleDidWake();
        break;
    default:
        break;
    }
}

static int moleStartSleepWatch(void) {
    gRootPort = IORegisterForSystemPower(NULL, &gNotifyPort, moleSleepCallback, &gNotifier);
    if (gRootPort == 0) {
        return -1;
    }
    gRunLoop = CFRunLoopGetCurrent();
    CFRunLoopAddSource(gRunLoop,
                       IONotificationPortGetRunLoopSource(gNotifyPort),
                       kCFRunLoopCommonModes);
    return 0;
}

static void moleRunSleepWatch(void) { CFRunLoopRun(); }

static void moleStopSleepWatch(void) {
    if (gRunLoop != NULL) {
        CFRunLoopStop(gRunLoop);
    }
}
*/
import "C"

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

var (
	sleepHookMu sync.Mutex
	sleepHook   func()
	wakeHook    func()
)

// lidClosed reports the machine's clamshell state: 1 shut, 0 open, -1 unknown.
// It is the gate the sleep callback applies, exposed so a test can check it
// against what the IO registry says out of band.
func lidClosed() int { return int(C.moleLidIsClosed()) }

//export moleWillSleep
func moleWillSleep() {
	sleepHookMu.Lock()
	fn := sleepHook
	sleepHookMu.Unlock()
	if fn != nil {
		fn()
	}
}

//export moleDidWake
func moleDidWake() {
	sleepHookMu.Lock()
	fn := wakeHook
	sleepHookMu.Unlock()
	if fn != nil {
		fn()
	}
}

// HoldAwake asks the system not to sleep until ReleaseAwake. Calls do not
// nest: a second Hold while one is held is a no-op, and one Release drops it.
//
// Only useful while the machine is in a wake it did not take for us — a dark
// wake — and honored only on AC power. A nil error means the assertion was
// created, not that the system will obey it; nothing in the API reports the
// difference. Callers must be able to lose the race.
func HoldAwake() error {
	if C.moleHoldAwake() != 0 {
		return fmt.Errorf("IOPMAssertionCreateWithName failed")
	}
	return nil
}

// ReleaseAwake drops the assertion taken by HoldAwake. Safe when none is held.
func ReleaseAwake() { C.moleReleaseAwake() }

// WatchPower calls onLidClose on each sleep that a closed lid caused and onWake
// on every wake, dark or full, and blocks until ctx is cancelled.
//
// Riding the sleep notification is the only reliable way to act on a closing
// lid. The lid state is readable (`AppleClamshellState` in the IO registry) but
// not pollable for this purpose: the machine sleeps a second or two after the
// lid shuts, so any polling interval fast enough to notice still leaves no time
// to finish a cross-border HTTPS request. The power-management notification is
// different in kind — the system asks us, and waits — so the registry read
// happens at the one moment it is both current and worth acting on.
//
// Two lid closes that this deliberately does not fire on: one with an external
// display attached and `AppleClamshellCausesSleep` off, because the machine
// stays awake and the tunnel stays wanted; and one on a desktop, which has no
// lid to close.
//
// onLidClose runs on the notification thread and blocks the machine's descent
// into sleep for as long as it takes, so it must be quick and must not
// deadlock. The ~30s ceiling is the system's, not ours: overrun it and the
// machine sleeps anyway, mid-request.
//
// onWake runs on the same thread and holds up nothing — the system does not
// wait on wake clients the way it waits on sleep clients. A wake handler that
// needs time has to take it with HoldAwake, and a dark wake will end under one
// that does not.
func WatchPower(ctx context.Context, onLidClose, onWake func()) error {
	sleepHookMu.Lock()
	sleepHook = onLidClose
	wakeHook = onWake
	sleepHookMu.Unlock()
	defer func() {
		sleepHookMu.Lock()
		sleepHook = nil
		wakeHook = nil
		sleepHookMu.Unlock()
	}()

	// The run loop is bound to whichever thread registered it, so this
	// goroutine must keep that thread for as long as it runs.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if C.moleStartSleepWatch() != 0 {
		return fmt.Errorf("IORegisterForSystemPower failed")
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		C.moleStopSleepWatch()
	}()

	C.moleRunSleepWatch()
	close(done)
	return nil
}
