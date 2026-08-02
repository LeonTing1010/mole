package core

import (
	"context"
	"log"
	"sync"
	"time"
)

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
func (s *Supervisor) runSleepDestroy(ctx context.Context) {
	destroyed := make(chan struct{})
	var once sync.Once

	go func() {
		defer recoverPanic("sleep-watch")
		err := WatchLidClose(ctx, func() {
			log.Printf("destroy-on-lid-close: the lid is shut — destroying the server")
			if err := s.onIdleDestroy(); err != nil {
				// Do not stall the machine retrying. It is going to sleep with
				// or without us, and the log says what was left behind.
				log.Printf("destroy-on-lid-close: destroy failed: %v — the server is STILL BILLING", err)
				return
			}
			log.Printf("destroy-on-lid-close: server destroyed; bring it back with `mole server deploy` then `mole up`")
			once.Do(func() { close(destroyed) })
		})
		if err != nil {
			log.Printf("destroy-on-lid-close: unavailable: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		return
	case <-s.stopCh:
		return
	case <-destroyed:
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
