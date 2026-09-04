package core

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/LeonTing1010/mole/utils"
)

// ExperimentArmTag names the probe-only outbound carrying one A/B arm.
//
// Deliberately outside the `proxy-` namespace the ladder and the standby leg
// use, because these outbounds are NOT selector members and must never carry
// user traffic: an arm exists to be measured, and routing real requests through
// an experimental port would make the experiment change the thing it measures.
func ExperimentArmTag(name string) string { return "ab-" + name }

// armOrder returns the order to probe arms in on one beat.
//
// Shuffled per beat, which matters more than it looks. Probing in declaration
// order gives the first arm a systematic handicap: it pays for whatever the
// path has to wake up, and the second rides the warmth. Over an evening that
// bias is indistinguishable from the effect under test — and an effect of the
// size worth acting on here is only a few percentage points of bad-dial rate.
func armOrder(n int, r *rand.Rand) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	r.Shuffle(n, func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	return idx
}

// ExperimentMinPaired is how many samples each arm needs before a difference is
// worth reading at all. Below this the confidence interval on a few-percent
// effect is wider than the effect.
const ExperimentMinPaired = 30

// CompareArms reports the bad-dial rates of two arms and the two-sided p-value
// of the difference under a two-proportion z-test.
//
// The test is here because the honest answer to "did that help?" has three
// values, not two, and every previous attempt on this tunnel collapsed it to
// two. Port hopping was adopted and then rejected on single evenings; obfs was
// deployed and its verdict is still ambiguous. A p-value will not make a small
// sample say something it cannot, which is exactly its job: it is the thing
// that says "keep it running" instead of letting an eager reading end the
// experiment early.
func CompareArms(a, b *utils.ExperimentArm) (rateA, rateB, p float64, enough bool) {
	na, nb := len(a.Samples), len(b.Samples)
	if na == 0 || nb == 0 {
		return 0, 0, 1, false
	}
	ba, bb := a.ArmBad(), b.ArmBad()
	rateA, rateB = float64(ba)/float64(na), float64(bb)/float64(nb)
	enough = na >= ExperimentMinPaired && nb >= ExperimentMinPaired
	pool := float64(ba+bb) / float64(na+nb)
	se := math.Sqrt(pool * (1 - pool) * (1/float64(na) + 1/float64(nb)))
	if se == 0 {
		// Both arms perfectly clean, or both perfectly broken: no difference to
		// test, and dividing by zero would report a spurious certainty.
		return rateA, rateB, 1, enough
	}
	z := (rateA - rateB) / se
	// Two-sided p from the standard normal tail.
	p = math.Erfc(math.Abs(z) / math.Sqrt2)
	return rateA, rateB, p, enough
}

// ExperimentVerdict renders the comparison as one line of English, including
// the case the numbers do not support a conclusion yet.
func ExperimentVerdict(a, b *utils.ExperimentArm) string {
	rateA, rateB, p, enough := CompareArms(a, b)
	switch {
	case !enough:
		return fmt.Sprintf("not enough yet — need %d samples per arm, have %d and %d",
			ExperimentMinPaired, len(a.Samples), len(b.Samples))
	case p > 0.05:
		return fmt.Sprintf("no difference worth acting on (%.0f%% vs %.0f%% bad, p=%.2f)",
			100*rateA, 100*rateB, p)
	case rateB < rateA:
		return fmt.Sprintf("%s is better: %.0f%% vs %.0f%% bad (p=%.3f)", b.Name, 100*rateB, 100*rateA, p)
	default:
		return fmt.Sprintf("%s is better: %.0f%% vs %.0f%% bad (p=%.3f)", a.Name, 100*rateA, 100*rateB, p)
	}
}
