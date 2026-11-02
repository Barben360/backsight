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

func TestEstimateDocumentCountsAtLeastOneFact(t *testing.T) {
	p := Prices{Input: 1, Output: 2}
	small := EstimateDocument(100, p)
	if want := EstimateSession(100, p).InputTokens + 1600; small.InputTokens != want {
		t.Errorf("small document input tokens = %d, want %d (one fact)", small.InputTokens, want)
	}
	if big := EstimateDocument(60000, p); big != EstimateSession(60000, p) {
		t.Errorf("large document = %+v, want the session estimate %+v", big, EstimateSession(60000, p))
	}
	if EstimatedDocumentFacts(100) != 1 || EstimatedDocumentFacts(0) != 0 || EstimatedDocumentFacts(1300) != 2 {
		t.Errorf("document facts = %d %d %d", EstimatedDocumentFacts(100), EstimatedDocumentFacts(0), EstimatedDocumentFacts(1300))
	}
}
