//go:build darwin

package core

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <CoreFoundation/CoreFoundation.h>

extern void moleWillSleep(void);

static io_connect_t          gRootPort;
static IONotificationPortRef gNotifyPort;
static io_object_t           gNotifier;
static CFRunLoopRef          gRunLoop;

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

// WatchLidClose calls onLidClose on each sleep that a closed lid caused, and
// blocks until ctx is cancelled.
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
func WatchLidClose(ctx context.Context, onLidClose func()) error {
	sleepHookMu.Lock()
	sleepHook = onLidClose
	sleepHookMu.Unlock()
	defer func() {
		sleepHookMu.Lock()
		sleepHook = nil
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
