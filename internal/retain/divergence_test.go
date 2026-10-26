package retain

import (
	"testing"
	"time"
)

func TestTheNoiseHistoryKeepsItsLastReadings(t *testing.T) {
	dir := t.TempDir()
	start := clockAt(0, 0)
	for i := range noiseKeep + 5 {
		if err := RecordNoise(dir, NoisePoint{At: start.Add(time.Duration(i) * time.Hour), Named: i, Total: 100}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadNoise(dir)
	if err != nil || len(got) != noiseKeep || got[0].Named != 5 || got[len(got)-1].Named != noiseKeep+4 {
		t.Fatalf("%d readings, first %+v, err %v", len(got), got[0], err)
	}
	if _, ok := WeekBefore(got, got[len(got)-1].At); ok {
		t.Error("readings of the last five days gave a week-old one")
	}
	if p, ok := WeekBefore([]NoisePoint{{At: start, Named: 1, Total: 4}}, start.Add(7*24*time.Hour)); !ok || p.Share() != 0.25 {
		t.Errorf("%+v %v", p, ok)
	}
}
