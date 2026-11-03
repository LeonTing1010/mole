package core

import "fmt"

// Negative share restores the legacy main selector; 0..8 allocates UDP lanes.
// Selector switches leave existing inbound connections on their original leg.
func ApplyBalancedLanes(c *ClashClient, hy2Lanes int) (bool, error) {
	if hy2Lanes < -1 || hy2Lanes > BalanceLanes {
		return false, fmt.Errorf("invalid UDP share %d", hy2Lanes)
	}
	changed := false
	for lane := 0; lane < BalanceLanes; lane++ {
		target := RealityLegTag
		if hy2Lanes < 0 {
			target = ProxySelectorTag
		} else if (lane+1)*hy2Lanes/BalanceLanes > lane*hy2Lanes/BalanceLanes {
			target = Hy2ProbeSelectorTag
		}
		tag := BalanceLaneTag(lane)
		live, err := c.Selection(tag)
		if err != nil {
			return changed, err
		}
		if live == target {
			continue
		}
		if err := c.SelectProxy(tag, target); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
