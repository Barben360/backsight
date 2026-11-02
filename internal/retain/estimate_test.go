package retain

import "testing"

func TestEstimatedFactsMatchesEstimateSession(t *testing.T) {
	p := Prices{Input: 1, Output: 2}
	for _, c := range []int64{0, 599, 600, 61234} {
		if got := EstimatedFacts(c); got != c/600 {
			t.Errorf("EstimatedFacts(%d) = %d, want %d", c, got, c/600)
		}
		chunks := (c + 29999) / 30000
		if chunks == 0 {
			chunks = 1
		}
		want := chunks*3300 + int64(float64(c)/3.5) + EstimatedFacts(c)*1600
		if e := EstimateSession(c, p); e.InputTokens != want {
			t.Errorf("EstimateSession(%d).InputTokens = %d, want %d", c, e.InputTokens, want)
		}
	}
}
