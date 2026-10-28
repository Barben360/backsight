package doctor

import "testing"

func TestVersionSkew(t *testing.T) {
	for _, c := range []struct {
		server string
		want   Status
	}{{"1.1.0", OK}, {"1.1.3", Info}, {"1.2.0", Warn}, {"2.0.0", Warn}, {"dev", Warn}, {"1.1", Warn}, {"1.1.x", Warn}} {
		if got := VersionSkew(c.server, "1.1.0"); got != c.want {
			t.Errorf("%s vs 1.1.0: %v, want %v", c.server, got, c.want)
		}
	}
}
