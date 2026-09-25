// Package sched picks which queue runs next.
//
// The rule (design §3.3): weighted round-robin across queues that are
// unpaused and non-empty, FIFO within a queue. A burst of twenty media adds
// must not starve one adhoc prompt sitting behind them.
package sched

import "sort"

// Candidate is one queue the scheduler may pick from.
type Candidate struct {
	Name   string
	Weight float64
	// Credit is the queue's persisted round-robin bookkeeping, so fairness
	// survives a restart rather than resetting to whoever submitted first.
	Credit float64
}

// Pick chooses the next queue using smooth weighted round-robin: every
// candidate gains its weight, the richest wins, and the winner pays the total
// weight back. Over time each queue runs in proportion to its weight, and the
// picks interleave rather than arriving in bursts.
//
// It returns the winner and the updated credits for every candidate. An empty
// candidate list yields "" and no credits.
func Pick(candidates []Candidate) (string, map[string]float64) {
	if len(candidates) == 0 {
		return "", nil
	}
	// Sort for determinism: equal credits must not depend on map iteration.
	sorted := make([]Candidate, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	credits := make(map[string]float64, len(sorted))
	var total float64
	best := -1
	for i := range sorted {
		sorted[i].Credit += sorted[i].Weight
		total += sorted[i].Weight
		if best < 0 || sorted[i].Credit > sorted[best].Credit {
			best = i
		}
	}
	sorted[best].Credit -= total
	for _, c := range sorted {
		credits[c.Name] = c.Credit
	}
	return sorted[best].Name, credits
}
