//go:build !darwin

package core

import (
	"context"
	"fmt"
)

// WatchPower is macOS-only: it rides IOKit's power-management notifications
// and the IO registry's clamshell state, neither of which has a portable
// equivalent. Elsewhere it reports that so the caller can leave
// destroy-on-lid-close off rather than silently arm a trigger that will never
// fire.
func WatchPower(ctx context.Context, onLidClose, onWake func()) error {
	return fmt.Errorf("destroy-on-lid-close is only implemented on macOS")
}

// HoldAwake and ReleaseAwake have nothing to hold open where WatchPower never
// fires. Declared so the destroy path compiles unchanged rather than growing a
// build tag of its own.
func HoldAwake() error { return nil }

func ReleaseAwake() {}

// lidClosed reports "cannot say" off darwin, which is the honest answer for a
// platform with no clamshell property to read.
func lidClosed() int { return -1 }
