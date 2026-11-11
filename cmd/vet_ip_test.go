package cmd

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestInterceptionVerdictFlagsPersistentLocalAnswers(t *testing.T) {
	var replies []time.Duration
	for i := 0; i < vetProbes; i++ {
		replies = append(replies, 400*time.Microsecond)
	}
	bad, why := interceptionVerdict(replies)
	if !bad {
		t.Fatalf("all-local answers should be intercepted, got clean: %s", why)
	}
}

func TestInterceptionVerdictPassesARealDistance(t *testing.T) {
	replies := make([]time.Duration, 0, vetProbes)
	for i := 0; i < vetProbes; i++ {
		replies = append(replies, 400*time.Microsecond)
	}
	replies[7] = 118 * time.Millisecond // one honest hop settles it
	bad, _ := interceptionVerdict(replies)
	if bad {
		t.Fatal("a reply at real distance means the host is genuinely reachable")
	}
}

func TestInterceptionVerdictIgnoresSilence(t *testing.T) {
	// A booting host answers nothing; that is the normal wait, not a verdict.
	bad, _ := interceptionVerdict(nil)
	if bad {
		t.Fatal("silence must not read as interception")
	}
}

func TestInterceptionVerdictWaitsForEnoughReplies(t *testing.T) {
	replies := []time.Duration{300 * time.Microsecond, 500 * time.Microsecond}
	bad, _ := interceptionVerdict(replies)
	if bad {
		t.Fatal("two fast replies could be a boot-time helper; below the reply floor must not convict")
	}
}

func TestIsInstanceLocked(t *testing.T) {
	if !isInstanceLocked(&ServerError{Method: "DELETE", Path: "/instances/x", Status: 409}) {
		t.Fatal("409 must read as locked")
	}
	if isInstanceLocked(&ServerError{Method: "DELETE", Path: "/instances/x", Status: 404}) {
		t.Fatal("404 is gone, not locked")
	}
	if isInstanceLocked(fmt.Errorf("transport: %w", errors.New("reset"))) {
		t.Fatal("a transport error says nothing about a lock")
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(1) {
		t.Fatal("launchd is certainly alive; EPERM from a root process must read as alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("invalid pids must not read as alive")
	}
}
