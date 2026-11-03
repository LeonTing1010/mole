package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/core"
)

// withServers points $HOME at a scratch directory and writes servers.json into
// it, so nothing here can touch the developer's live ~/.mole.
func withServers(t *testing.T, servers ...Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if len(servers) > 0 {
		if err := saveServers(servers); err != nil {
			t.Fatalf("write servers.json: %v", err)
		}
	}
}

// withAccount replaces the account read for the duration of one test.
func withAccount(t *testing.T, instances []serverInstance, err error) {
	t.Helper()
	prev := listInstancesForDestroy
	listInstancesForDestroy = func(time.Duration) ([]serverInstance, error) { return instances, err }
	t.Cleanup(func() { listInstancesForDestroy = prev })
}

// The phantom debt of 2026-08-15, from the side that invented it. A lid close at
// 12:45 destroyed the server and emptied servers.json; the lid closed again at
// 14:02 and the hook — closed over the name from `mole up` — returned "no
// servers" in 1ms. That error became a debt, and `mole status` reported a VPS
// billing unattended for the rest of the afternoon while the account held one
// instance, created afterwards.
//
// Nothing local and nothing on the account is not a failed destroy. It is the
// state a lid close is trying to reach.
func TestNothingLocalAndAnEmptyAccountIsNothingToDestroy(t *testing.T) {
	withServers(t)
	withAccount(t, nil, nil)

	if err := destroyActiveServerWithin(time.Second); !errors.Is(err, core.ErrNothingToDestroy) {
		t.Errorf("destroyActiveServerWithin = %v, want ErrNothingToDestroy — anything else becomes a debt for a server that does not exist", err)
	}
}

// The inverse, and the reason the account is consulted at all: the local entry
// can be lost while the instance keeps billing. Reporting "nothing to destroy"
// there would silence the one warning that matters, so this must stay an error —
// and it must point at the route that can actually delete an orphan, which needs
// only the API key.
func TestNothingLocalButAnOrphanOnTheAccountStaysADebt(t *testing.T) {
	withServers(t)
	withAccount(t, []serverInstance{{ID: "cf7674ee", Label: "icn-1786758841", MainIP: "198.51.100.10"}}, nil)

	err := destroyActiveServerWithin(time.Second)
	if err == nil || errors.Is(err, core.ErrNothingToDestroy) {
		t.Fatalf("destroyActiveServerWithin = %v, want a real error — an orphan is billing", err)
	}
	if !strings.Contains(err.Error(), "icn-1786758841") {
		t.Errorf("error %q does not name the instance that is billing", err)
	}
	if !strings.Contains(err.Error(), "--destroy-remote") {
		t.Errorf("error %q does not point at the only route that can destroy an orphan without the hy2 password", err)
	}
}

// "Nothing to destroy" is a claim about the account, so it may only be made when
// the account answered. An unreachable API is exactly the condition a lid close
// hits on a dark path — the case the debt exists for — and treating silence as
// "empty" would drop the record every time it was most needed.
func TestAnUnreachableAccountIsNeverReadAsEmpty(t *testing.T) {
	withServers(t)
	withAccount(t, nil, errors.New("context deadline exceeded"))

	err := destroyActiveServerWithin(time.Second)
	if err == nil || errors.Is(err, core.ErrNothingToDestroy) {
		t.Fatalf("destroyActiveServerWithin = %v, want a real error when the account could not be reached", err)
	}
}

// The daemon's hook used to close over the server that was active when `mole up`
// ran. Resolving it per call is what keeps a second lid close pointed at the
// server that actually exists — a deploy between lid closes changes the answer.
func TestTheActiveServerIsResolvedAtCallTime(t *testing.T) {
	withServers(t,
		Server{Name: "icn-old", Region: "icn", IP: "10.0.0.1"},
		Server{Name: "icn-new", Region: "icn", IP: "10.0.0.2", Active: true},
	)
	withAccount(t, nil, errors.New("must not be consulted while a local entry exists"))

	// No InstanceID on either entry, so the destroy is local-only and no
	// management call is made.
	if err := destroyActiveServerWithin(time.Second); err != nil {
		t.Fatalf("destroyActiveServerWithin: %v", err)
	}

	left, err := loadServers()
	if err != nil {
		t.Fatalf("read servers: %v", err)
	}
	if len(left) != 1 || left[0].Name != "icn-old" {
		t.Fatalf("destroyed the wrong server; servers.json now holds %+v", left)
	}
}

// Entries with none marked active is an ambiguous state, and every resolution of
// it costs money in one direction or the other: destroying a guess deletes a
// server someone may want, skipping silently leaves one billing. Say what is
// there instead.
func TestNoActiveServerIsReportedRatherThanGuessed(t *testing.T) {
	withServers(t,
		Server{Name: "icn-a", Region: "icn", IP: "10.0.0.1"},
		Server{Name: "icn-b", Region: "icn", IP: "10.0.0.2"},
	)
	withAccount(t, nil, nil)

	err := destroyActiveServerWithin(time.Second)
	if err == nil || errors.Is(err, core.ErrNothingToDestroy) {
		t.Fatalf("destroyActiveServerWithin = %v, want a real error naming the ambiguity", err)
	}
	if left, _ := loadServers(); len(left) != 2 {
		t.Errorf("guessed and destroyed something: servers.json now holds %+v", left)
	}
}
