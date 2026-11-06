package machines

// Bucket returns the latency bucket (0..8) of ms: bucket i holds
// Edges[i-1] < ms <= Edges[i]; bucket 0 holds ms <= 50, bucket 8 ms > 5000.
func Bucket(ms int64) int {
	for i, e := range Edges {
		if ms <= e {
			return i
		}
	}
	return len(Edges)
}

// Rank returns the bucket holding the nearest rank ceil(p*n/100) of the
// counts b (n = their sum); ok is false when n is 0.
func Rank(b [9]int64, p int) (bucket int, ok bool) {
	var n int64
	for _, c := range b {
		n += c
	}
	if n == 0 {
		return 0, false
	}
	rank := (int64(p)*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	var cum int64
	for i, c := range b {
		cum += c
		if cum >= rank {
			return i, true
		}
	}
	return len(b) - 1, true
}

// Range returns the edges of a bucket as (lo, hi]; hi is nil for the open bucket 8.
func Range(bucket int) (lo int64, hi *int64) {
	if bucket > 0 {
		lo = Edges[bucket-1]
	}
	if bucket < len(Edges) {
		h := Edges[bucket]
		hi = &h
	}
	return lo, hi
}
