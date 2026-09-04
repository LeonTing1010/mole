package core

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/LeonTing1010/mole/utils"
)

func arm(name string, bad, good int) *utils.ExperimentArm {
	a := &utils.ExperimentArm{Name: name}
	for i := 0; i < bad; i++ {
		a.Record(0, errors.New("timeout"))
	}
	for i := 0; i < good; i++ {
		a.Record(300, nil)
	}
	return a
}

// The whole tool exists to stop a small sample from ending an experiment early,
// because that is how port hopping and obfs were both judged. A difference that
// looks large in 10 beats must not be reported as a finding.
func TestCompareRefusesToCallASmallSample(t *testing.T) {
	a, b := arm("control", 8, 2), arm("test", 1, 9)
	if _, _, _, enough := CompareArms(a, b); enough {
		t.Fatal("10 samples per arm must not be enough to call anything")
	}
	got := ExperimentVerdict(a, b)
	if got == "" || !contains(got, "not enough yet") {
		t.Fatalf("verdict should say the sample is too small, got %q", got)
	}
}

// The measured shape this is built for: an arm that is dramatically better over
// a real window should be called, and named.
func TestCompareCallsALargeRealDifference(t *testing.T) {
	control, test := arm("p443", 44, 56), arm("p20443", 4, 96)
	rateA, rateB, p, enough := CompareArms(control, test)
	if !enough {
		t.Fatal("100 samples per arm is enough to call")
	}
	if rateA <= rateB {
		t.Fatalf("control should be the worse arm: %.2f vs %.2f", rateA, rateB)
	}
	if p > 0.001 {
		t.Fatalf("44%% vs 4%% over 100 samples should be decisive, got p=%.4f", p)
	}
	if got := ExperimentVerdict(control, test); !contains(got, "p20443 is better") {
		t.Fatalf("verdict should name the better arm, got %q", got)
	}
}

// Two arms that are the same must read as the same. This is the answer the
// experiment is MOST likely to produce and the one most likely to be
// over-read — a few percentage points of difference between identical arms is
// what random sampling does.
func TestCompareReportsNoDifferenceWhenThereIsNone(t *testing.T) {
	a, b := arm("a", 20, 80), arm("b", 23, 77)
	if got := ExperimentVerdict(a, b); !contains(got, "no difference worth acting on") {
		t.Fatalf("20%% vs 23%% over 100 samples is noise, got %q", got)
	}
}

// Both arms perfectly clean is a real outcome on a good evening, and the
// pooled standard error is zero there. It must report "no difference", not
// divide by zero into a spurious certainty.
func TestCompareHandlesTwoPerfectArms(t *testing.T) {
	a, b := arm("a", 0, 50), arm("b", 0, 50)
	_, _, p, enough := CompareArms(a, b)
	if !enough {
		t.Fatal("50 samples per arm is enough")
	}
	if p != 1 {
		t.Fatalf("two identical clean arms have no difference to test, got p=%.4f", p)
	}
}

// Order must cover every arm exactly once, and must not always be the
// declaration order — the bias it removes (the first probe pays for waking the
// path) is the same size as the effect under test.
func TestArmOrderCoversAndShuffles(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	identical := 0
	for i := 0; i < 50; i++ {
		got := armOrder(3, r)
		seen := map[int]bool{}
		for _, v := range got {
			if v < 0 || v > 2 {
				t.Fatalf("index %d out of range", v)
			}
			if seen[v] {
				t.Fatalf("index %d probed twice in one beat: %v", v, got)
			}
			seen[v] = true
		}
		if len(seen) != 3 {
			t.Fatalf("beat did not cover every arm: %v", got)
		}
		if got[0] == 0 && got[1] == 1 && got[2] == 2 {
			identical++
		}
	}
	if identical == 50 {
		t.Fatal("order never changed — the first arm would carry a systematic handicap")
	}
}

// The rolling window must not grow without bound: this runs for hours.
func TestArmWindowIsBounded(t *testing.T) {
	a := arm("x", 0, utils.ExperimentWindow*3)
	if len(a.Samples) != utils.ExperimentWindow {
		t.Fatalf("window should cap at %d, got %d", utils.ExperimentWindow, len(a.Samples))
	}
}

// A slow-but-completed dial is bad. The failure this measures shows up as
// seconds, not as errors, so an arm that only counted hard failures would miss
// most of the episode — 12 of the 16 bad beats on 2026-09-01 completed.
func TestArmCountsSlowDialsAsBad(t *testing.T) {
	a := &utils.ExperimentArm{Name: "x"}
	a.Record(utils.DialSlowMs+500, nil)
	a.Record(300, nil)
	if a.ArmBad() != 1 {
		t.Fatalf("a 1.5s dial that completed is still bad, got %d bad", a.ArmBad())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
