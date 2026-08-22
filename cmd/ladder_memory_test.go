package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// The ladder record has one job: outlive the server that carried it. Destroy
// deletes the entry, so without this every deploy re-inherits the timid
// defaults and a measured path runs at a fraction of what it can do.
func TestLadderMemorySurvivesTheServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	measured := Server{
		Name: "icn-1", Region: "icn",
		UpMbps: 10, DownMbps: 65,
		PeakUpMbps: 5, PeakDownMbps: 50,
		PeakStartHour: 12, PeakEndHour: 2,
	}
	rememberLadder(measured)

	got, ok := recallLadder("icn")
	if !ok {
		t.Fatal("nothing recalled for the region that was just recorded")
	}
	if got.DownMbps != 65 || got.UpMbps != 10 {
		t.Errorf("off-peak ceiling came back as %d↓/%d↑, want 65/10", got.DownMbps, got.UpMbps)
	}
	if got.PeakDownMbps != 50 || got.PeakUpMbps != 5 {
		t.Errorf("peak ceiling came back as %d↓/%d↑, want 50/5", got.PeakDownMbps, got.PeakUpMbps)
	}
	if got.PeakStartHour != 12 || got.PeakEndHour != 2 {
		t.Errorf("peak window came back as %d→%d, want 12→2", got.PeakStartHour, got.PeakEndHour)
	}
}

// A ceiling describes a path, not a box. Regions ride different transit — a
// same-evening bake-off has shown one at 0% loss and another at 6% — so a
// number measured in Seoul must not follow a deploy to Tokyo.
func TestLadderMemoryDoesNotCrossRegions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rememberLadder(Server{Name: "icn-1", Region: "icn", DownMbps: 65, UpMbps: 10, PeakDownMbps: 50, PeakUpMbps: 5})

	if _, ok := recallLadder("nrt"); ok {
		t.Error("a Seoul measurement was offered to a Tokyo deploy")
	}
	if _, ok := recallLadder(""); ok {
		t.Error("an empty region matched a record")
	}
}

// Regions are re-measured over time, and the last word wins — including when
// the last word is lower, which is how a path that got worse gets recorded.
func TestLadderMemoryOverwritesPerRegion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rememberLadder(Server{Name: "icn-1", Region: "icn", DownMbps: 65, UpMbps: 10, PeakDownMbps: 50, PeakUpMbps: 5})
	rememberLadder(Server{Name: "icn-2", Region: "icn", DownMbps: 20, UpMbps: 5, PeakDownMbps: 5, PeakUpMbps: 2})
	rememberLadder(Server{Name: "nrt-1", Region: "nrt", DownMbps: 12, UpMbps: 3, PeakDownMbps: 3, PeakUpMbps: 1})

	icn, _ := recallLadder("icn")
	if icn.DownMbps != 20 {
		t.Errorf("icn kept %d, want the later record's 20", icn.DownMbps)
	}
	nrt, ok := recallLadder("nrt")
	if !ok || nrt.DownMbps != 12 {
		t.Errorf("nrt = %d (recorded %v), want its own 12 — regions must not overwrite each other", nrt.DownMbps, ok)
	}
}

// A server with nowhere to attribute the measurement teaches nothing.
func TestLadderMemoryIgnoresRegionlessServers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rememberLadder(Server{Name: "imported", DownMbps: 65})

	m, err := loadLadderMemory()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("recorded %d entries for a server with no region, want 0", len(m))
	}
}

// A deploy that cannot read the record must still deploy. Falling back to the
// defaults costs throughput; refusing to provision costs the whole command.
func TestLadderMemoryTolerantOfCorruption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := os.WriteFile(utils.LadderMemoryPath(), []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := recallLadder("icn"); ok {
		t.Error("a corrupt record was reported as usable")
	}
	// And it must be repairable by the next destroy rather than staying broken.
	rememberLadder(Server{Name: "icn-1", Region: "icn", DownMbps: 65, UpMbps: 10, PeakDownMbps: 50, PeakUpMbps: 5})
	if l, ok := recallLadder("icn"); !ok || l.DownMbps != 65 {
		t.Error("a corrupt record was not overwritten by the next measurement")
	}
}

// An operator reading the deploy line needs to know whether the number is from
// yesterday or from last spring.
func TestDescribeAge(t *testing.T) {
	now := time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC)
	at := func(d time.Duration) regionLadder {
		return regionLadder{RememberedAt: now.Add(-d).Format(time.RFC3339)}
	}
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Minute, "recorded minutes ago"},
		{5 * time.Hour, "recorded 5h ago"},
		{30 * 24 * time.Hour, "recorded 30d ago"},
	} {
		if got := describeAge(at(tc.ago), now); got != tc.want {
			t.Errorf("%v ago rendered as %q, want %q", tc.ago, got, tc.want)
		}
	}
	// A record written before the timestamp existed still has to render.
	if got := describeAge(regionLadder{}, now); got == "" {
		t.Error("a record with no timestamp rendered as empty")
	}
}

// Port hopping has to survive the URI round trip, because the URI is the only
// channel between servers.json and the sing-box config. It must also stay
// absent for servers that never got a range — emitting ?ports= for those would
// point the client at ports the node's firewall drops.
func TestURICarriesPortHopping(t *testing.T) {
	s := Server{
		Name: "icn-1", IP: "1.2.3.4", Port: 443, Password: "pw",
		UpMbps: 5, DownMbps: 65,
		HopPorts: "20000-50000", HopSeconds: 30,
	}
	uri := s.URI()
	if !strings.Contains(uri, "ports=20000-50000") {
		t.Errorf("URI dropped the hop range: %s", uri)
	}
	if !strings.Contains(uri, "hop=30") {
		t.Errorf("URI dropped the hop interval: %s", uri)
	}
	// The base port must still be the URI's port: it is what the server always
	// listens on and the fallback if hopping is ever turned off.
	if !strings.Contains(uri, "@1.2.3.4:443?") {
		t.Errorf("URI lost the base port: %s", uri)
	}

	s.HopPorts, s.HopSeconds = "", 0
	if plain := s.URI(); strings.Contains(plain, "ports=") || strings.Contains(plain, "hop=") {
		t.Errorf("a server with no range must not advertise one: %s", plain)
	}
}
