package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// What a path was measured to carry, remembered per region so it survives the
// server that taught us.
//
// The problem this solves is specific to how mole is used. Destroy-on-lid-close
// means a new instance most mornings, `mole server deploy` writes the
// conservative defaults into every new entry, and destroy deletes the old entry
// outright — so an afternoon spent laddering a path up to its real ceiling is
// gone by the next day, and the tunnel silently runs at a fraction of what the
// link can do. (Measured once: a peak ceiling of 5 Mbps inherited from a
// different, worse region, on a path that carried 60.)
//
// Regions are the unit because the path is what is being remembered, not the
// box. A new instance in the same region rides the same transit; one in another
// region does not, and a bake-off between regions has shown swings from 0% to
// 6% loss on the same evening. So nothing is inherited across a region change.
//
// Being wrong here is survivable in the direction that matters: the supervisor
// backs the ceiling down a rung at a time when the path misbehaves, so an
// inherited ceiling that the new box cannot hold gets corrected within minutes.
// An inherited ceiling that is too LOW has nothing to correct it, which is the
// asymmetry that argues for remembering at all.
type regionLadder struct {
	UpMbps          int `json:"up_mbps"`
	DownMbps        int `json:"down_mbps"`
	PeakUpMbps      int `json:"peak_up_mbps"`
	PeakDownMbps    int `json:"peak_down_mbps"`
	RealityDownMbps int `json:"reality_down_mbps,omitempty"`
	// RealityRateAuto is remembered alongside the rates: it is the operator's
	// choice about who owns the REALITY leg's declaration, not a property of
	// the instance, and losing it on redeploy silently reverts the leg to a
	// pinned rate — which is exactly the stale-declaration failure the
	// governor exists to prevent.
	RealityRateAuto bool   `json:"reality_rate_auto,omitempty"`
	PeakStartHour   int    `json:"peak_start_hour"`
	PeakEndHour     int    `json:"peak_end_hour"`
	RememberedAt    string `json:"remembered_at"`
}

// loadLadderMemory reads the per-region record. A missing file is not an error
// — it is the state every machine starts in.
func loadLadderMemory() (map[string]regionLadder, error) {
	b, err := os.ReadFile(utils.LadderMemoryPath())
	if os.IsNotExist(err) {
		return map[string]regionLadder{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]regionLadder{}
	if err := json.Unmarshal(b, &m); err != nil {
		// A corrupt record is worth strictly less than a working deploy. Report
		// it, but let the caller fall back to the defaults rather than refusing
		// to provision a server over it.
		return nil, fmt.Errorf("read %s: %w", utils.LadderMemoryPath(), err)
	}
	return m, nil
}

func saveLadderMemory(m map[string]regionLadder) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(utils.LadderMemoryPath(), append(b, '\n'), 0644); err != nil {
		return err
	}
	// The daemon destroys servers as root, so it writes this file as root too.
	utils.ChownToMoleDirOwner(utils.LadderMemoryPath())
	return nil
}

// rememberLadder records what a server's region was running, keyed by region.
//
// Called on the way out of destroy, because that is the last moment the
// knowledge exists: the entry is about to be deleted, and with it every number
// that took a measurement session to find.
//
// A region with no ceiling worth recording is skipped rather than stored as
// zeros — an entry that says nothing would still shadow the defaults.
func rememberLadder(s Server) {
	if s.Region == "" {
		return
	}
	up, down, peakUp, peakDown, start, end := s.ladder()
	if down <= 0 {
		return
	}
	m, err := loadLadderMemory()
	if err != nil {
		// Losing the record is not worth failing a destroy over — the instance
		// is already gone on the remote side.
		m = map[string]regionLadder{}
	}
	m[s.Region] = regionLadder{
		UpMbps: up, DownMbps: down,
		PeakUpMbps: peakUp, PeakDownMbps: peakDown,
		RealityDownMbps: s.RealityDownMbps,
		RealityRateAuto: s.RealityRateAuto,
		PeakStartHour:   start, PeakEndHour: end,
		RememberedAt: time.Now().Format(time.RFC3339),
	}
	if err := saveLadderMemory(m); err != nil {
		fmt.Printf("⚠️  Could not remember %s's ceiling for region %s: %v\n", s.Name, s.Region, err)
	}
}

// recallLadder returns what this region was last measured to carry, and whether
// anything was on record.
func recallLadder(region string) (regionLadder, bool) {
	if region == "" {
		return regionLadder{}, false
	}
	m, err := loadLadderMemory()
	if err != nil {
		fmt.Printf("⚠️  %v — deploying with the default ceiling\n", err)
		return regionLadder{}, false
	}
	l, ok := m[region]
	if !ok || l.DownMbps <= 0 {
		return regionLadder{}, false
	}
	return l, true
}

// describeAge renders how long ago a remembered ladder was recorded, for the
// one line the deploy prints about where its ceiling came from. It says
// "recorded", not "measured": the record is whatever the last server ran with,
// which may be a hand-tuned ceiling or may be the untouched defaults. An
// operator who sees a six-month-old number has enough to decide whether to
// re-measure.
func describeAge(l regionLadder, now time.Time) string {
	t, err := time.Parse(time.RFC3339, l.RememberedAt)
	if err != nil {
		return "recorded earlier"
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return "recorded minutes ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("recorded %dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("recorded %dd ago", int(d.Hours()/24))
	}
}
