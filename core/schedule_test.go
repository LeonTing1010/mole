package core

import (
	"reflect"
	"testing"
	"time"
)

// The nrt server's real shape as of 2026-07-21: off-peak 5/20, peak 2/5,
// window 18:00–00:00.
func nrtSchedule() BandwidthSchedule {
	return BandwidthSchedule{
		OffUp: 5, OffDown: 20,
		PeakUp: 2, PeakDown: 5,
		StartHour: 18, EndHour: 0,
	}
}

func TestRungs(t *testing.T) {
	tests := []struct {
		name string
		s    BandwidthSchedule
		want []int
	}{
		{
			name: "nrt: standard rungs fill in below the off-peak max",
			s:    nrtSchedule(),
			want: []int{1, 2, 3, 5, 8, 12, 20},
		},
		{
			name: "no peak profile still ladders, so a manual pin has somewhere to land",
			s:    BandwidthSchedule{OffUp: 5, OffDown: 20},
			want: []int{1, 2, 3, 5, 8, 12, 20},
		},
		{
			name: "a non-standard peak ceiling is still a rung",
			s:    BandwidthSchedule{OffUp: 5, OffDown: 20, PeakUp: 2, PeakDown: 7},
			want: []int{1, 2, 3, 5, 7, 8, 12, 20},
		},
		{
			// Guards the bug where PeakDown was filtered out by a `<= OffDown`
			// check while Member() still named it — the scheduler would have
			// selected an outbound the config never defined.
			name: "peak above off-peak still yields both clock-reachable rungs",
			s:    BandwidthSchedule{OffUp: 2, OffDown: 5, PeakUp: 5, PeakDown: 20},
			want: []int{1, 2, 3, 5, 8, 12, 20},
		},
		{
			name: "BBR mode has no ceiling to ladder",
			s:    BandwidthSchedule{OffUp: 0, OffDown: 0},
			want: nil,
		},
		{
			name: "a ceiling at the bottom rung collapses the ladder",
			s:    BandwidthSchedule{OffUp: 1, OffDown: 1},
			want: []int{1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.s.Rungs()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Rungs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every ceiling the clock can select must exist as a rung, or the supervisor
// would PUT a selector member sing-box has never heard of.
func TestMemberIsAlwaysARung(t *testing.T) {
	schedules := []BandwidthSchedule{
		nrtSchedule(),
		{OffUp: 5, OffDown: 20},
		{OffUp: 5, OffDown: 20, PeakUp: 2, PeakDown: 7, StartHour: 18, EndHour: 0},
		{OffUp: 2, OffDown: 5, PeakUp: 5, PeakDown: 20, StartHour: 1, EndHour: 6},
	}
	for _, s := range schedules {
		for h := 0; h < 24; h++ {
			at := time.Date(2026, 7, 21, h, 30, 0, 0, time.Local)
			_, _, down := s.Profile(at)
			if !s.HasRung(down) {
				t.Errorf("schedule %+v at %02d:30 selects %d Mbps, which is not in Rungs()=%v", s, h, down, s.Rungs())
			}
			if want := BandwidthRungTag(down); s.Member(at) != want {
				t.Errorf("Member() = %q, want %q", s.Member(at), want)
			}
		}
	}
}

func TestRungUp(t *testing.T) {
	s := nrtSchedule()
	tests := []struct {
		down, want int
		why        string
	}{
		{20, 5, "off-peak pair is used verbatim"},
		{5, 2, "peak pair is used verbatim, not the 1:4 off-peak ratio"},
		{12, 3, "intermediate rung inherits the off-peak ratio (12*5/20=3)"},
		{8, 2, "intermediate rung inherits the off-peak ratio (8*5/20=2)"},
		{1, 1, "never 0 — a zero up_mbps would drop the rung out of Brutal into BBR"},
		{2, 1, "rounds down but floors at 1 (2*5/20=0 → 1)"},
	}
	for _, tc := range tests {
		if got := s.RungUp(tc.down); got != tc.want {
			t.Errorf("RungUp(%d) = %d, want %d — %s", tc.down, got, tc.want, tc.why)
		}
	}
}

// A rung must never be handed to sing-box with a zero up_mbps, which would
// silently disable Brutal for that ceiling.
func TestRungUpNeverZero(t *testing.T) {
	schedules := []BandwidthSchedule{
		nrtSchedule(),
		{OffUp: 1, OffDown: 50},
		{OffUp: 5, OffDown: 20, PeakUp: 0, PeakDown: 5},
		{OffUp: 0, OffDown: 20},
	}
	for _, s := range schedules {
		for _, down := range s.Rungs() {
			if up := s.RungUp(down); up <= 0 {
				t.Errorf("schedule %+v: RungUp(%d) = %d, want > 0", s, down, up)
			}
		}
	}
}

func TestProfileWindowWrapsMidnight(t *testing.T) {
	s := nrtSchedule() // 18:00 → 00:00
	at := func(h int) time.Time { return time.Date(2026, 7, 21, h, 30, 0, 0, time.Local) }
	for _, h := range []int{18, 21, 23} {
		if name, _, _ := s.Profile(at(h)); name != "peak" {
			t.Errorf("%02d:30 → %q, want peak", h, name)
		}
	}
	for _, h := range []int{0, 9, 17} {
		if name, _, _ := s.Profile(at(h)); name != "offpeak" {
			t.Errorf("%02d:30 → %q, want offpeak", h, name)
		}
	}
}
