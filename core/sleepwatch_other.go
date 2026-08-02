//go:build !darwin

package core

import (
	"context"
	"fmt"
)

// WatchLidClose is macOS-only: it rides IOKit's power-management notifications
// and the IO registry's clamshell state, neither of which has a portable
// equivalent. Elsewhere it reports that so the caller can leave
// destroy-on-lid-close off rather than silently arm a trigger that will never
// fire.
func WatchLidClose(ctx context.Context, onLidClose func()) error {
	return fmt.Errorf("destroy-on-lid-close is only implemented on macOS")
}
