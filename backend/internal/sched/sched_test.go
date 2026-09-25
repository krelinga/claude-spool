package sched

import (
	"math"
	"testing"
)

// run simulates n picks over a fixed candidate set, carrying credits forward
// the way the executor does.
func run(t *testing.T, cands []Candidate, n int) ([]string, map[string]int) {
	t.Helper()
	credits := map[string]float64{}
	counts := map[string]int{}
	var order []string
	for range n {
		round := make([]Candidate, len(cands))
		for i, c := range cands {
			c.Credit = credits[c.Name]
			round[i] = c
		}
		winner, updated := Pick(round)
		if winner == "" {
			t.Fatal("Pick returned no winner")
		}
		credits = updated
		counts[winner]++
		order = append(order, winner)
	}
	return order, counts
}

func TestPickEmpty(t *testing.T) {
	if name, credits := Pick(nil); name != "" || credits != nil {
		t.Errorf("Pick(nil) = %q, %v", name, credits)
	}
}

func TestSingleCandidateAlwaysWins(t *testing.T) {
	_, counts := run(t, []Candidate{{Name: "media", Weight: 1}}, 5)
	if counts["media"] != 5 {
		t.Errorf("counts = %v", counts)
	}
}

// Equal weights must alternate, not run one queue to exhaustion.
func TestEqualWeightsAlternate(t *testing.T) {
	order, counts := run(t, []Candidate{
		{Name: "adhoc", Weight: 1},
		{Name: "media", Weight: 1},
	}, 6)
	if counts["media"] != 3 || counts["adhoc"] != 3 {
		t.Errorf("counts = %v, want even split", counts)
	}
	for i := 1; i < len(order); i++ {
		if order[i] == order[i-1] {
			t.Errorf("equal weights produced a run of %q at %d: %v", order[i], i, order)
			break
		}
	}
}

// The design's motivating case: a heavy queue must not starve a light one.
func TestHeavyQueueDoesNotStarveLightOne(t *testing.T) {
	order, counts := run(t, []Candidate{
		{Name: "media", Weight: 10},
		{Name: "adhoc", Weight: 1},
	}, 22)
	if counts["adhoc"] != 2 {
		t.Errorf("adhoc ran %d times in 22 picks, want 2", counts["adhoc"])
	}
	// The light queue's turns must be spread out, not both at the very end.
	var gap int
	for i, q := range order {
		if q == "adhoc" {
			gap = i
			break
		}
	}
	if gap > 11 {
		t.Errorf("adhoc waited %d picks for its first turn: %v", gap, order)
	}
}

func TestWeightsAreProportional(t *testing.T) {
	const n = 1200
	_, counts := run(t, []Candidate{
		{Name: "a", Weight: 3},
		{Name: "b", Weight: 2},
		{Name: "c", Weight: 1},
	}, n)
	for name, share := range map[string]float64{"a": 0.5, "b": 1.0 / 3, "c": 1.0 / 6} {
		got := float64(counts[name]) / n
		if math.Abs(got-share) > 0.01 {
			t.Errorf("%s got %.3f of picks, want %.3f (counts %v)", name, got, share, counts)
		}
	}
}

// A queue that goes quiet, then comes back, must not bank unlimited credit and
// then monopolise the executor.
func TestIdleQueueDoesNotBankCredit(t *testing.T) {
	credits := map[string]float64{}
	// Only media is runnable for a while.
	for range 10 {
		_, credits = Pick([]Candidate{{Name: "media", Weight: 1, Credit: credits["media"]}})
	}
	// adhoc returns; it should win soon, but not ten times in a row.
	counts := map[string]int{}
	for range 10 {
		var round []Candidate
		for _, c := range []Candidate{{Name: "media", Weight: 1}, {Name: "adhoc", Weight: 1}} {
			c.Credit = credits[c.Name]
			round = append(round, c)
		}
		var winner string
		winner, credits = Pick(round)
		counts[winner]++
	}
	if counts["media"] == 0 {
		t.Errorf("returning queue monopolised the executor: %v", counts)
	}
}

func TestPickIsDeterministic(t *testing.T) {
	cands := []Candidate{{Name: "b", Weight: 1}, {Name: "a", Weight: 1}, {Name: "c", Weight: 1}}
	first, _ := Pick(cands)
	for range 20 {
		if got, _ := Pick(cands); got != first {
			t.Fatalf("Pick not deterministic: %q then %q", first, got)
		}
	}
}
