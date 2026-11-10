package splash

import "testing"

// BenchmarkBuild measures the panel against its 50 ms budget (decision
// 0084 D4); no test gates on wall-clock time.
func BenchmarkBuild(b *testing.B) {
	src := fixture(b)
	for b.Loop() {
		Build(src)
	}
}
