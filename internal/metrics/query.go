package metrics

// Query is what a view is asked: a window and the filters of the browse
// views. Zero values mean "no filter".
type Query struct {
	Window Window
	// Project filters by the session's project; Path by recall path.
	Project, Path string
	// Machines is "this" (block 8 adds "all").
	Machines string
	// Client keeps the activity of one coding tool (a harnessid id): the
	// rows of the index tables that record it (clientTables), this PC's
	// alone (other PCs send no client). "" keeps every one.
	Client string
	// Sort, State, ID and Has serve the browse views.
	Sort, State, ID, Has string
	// Limit is the page size (DefaultLimit when 0, at most MaxLimit).
	Limit, Offset int
	// Dev adds the developer-only metrics; Text asks for a memory's text.
	Dev, Text bool
	// WithOwn asks a merged run (Machines "all") to carry this PC's own answer too (Result.ThisPC): what
	// `--machines this` gives for the same query, from the same read. Unset, the CLI pays nothing for it.
	WithOwn bool
	// Compare and Measure serve the releases view: the two versions to
	// compare and the measure (used.turn_rate when empty).
	Compare [2]string
	Measure string
	// version restricts the reach, quality and cost rows behind the
	// releases view's flagship metrics to one binary version (each row by
	// its own version; a journal row by its turn's).
	version string
}

// Paging limits of the browse views.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// EffectiveLimit is Limit within [1, MaxLimit], DefaultLimit when unset.
func (q Query) EffectiveLimit() int {
	switch {
	case q.Limit <= 0:
		return DefaultLimit
	case q.Limit > MaxLimit:
		return MaxLimit
	}
	return q.Limit
}
