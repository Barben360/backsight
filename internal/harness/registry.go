package harness

import (
	"context"
	"slices"
	"sync"
)

var (
	mu       sync.RWMutex
	adapters = []Adapter{Claude{}}
	// known are the adapters ByID finds that are not on yet: a command
	// line naming one (`hook --harness <id>`, `mcp --harness <id>`)
	// reaches it, but nothing iterates it (All) and nothing defaults to
	// it. A harness moves from here to adapters once its blocks land
	// (Codex's doc says when).
	known = []Adapter{Codex{}}
)

// All are the adapters that are on, Claude Code's first, then in the
// order they were registered: those whose conversations backsight reads.
func All() []Adapter {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Clone(adapters)
}

// ByID is the adapter registered under id, one not on yet included.
func ByID(id string) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, a := range slices.Concat(adapters, known) {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}

// Default is the adapter of code that names no harness: Claude Code's.
func Default() Adapter { return Claude{} }

// Of is the adapter registered under id, Default for "" (a record written
// before harnesses had ids) or an id no longer registered.
func Of(id string) Adapter {
	if a, ok := ByID(id); ok {
		return a
	}
	return Default()
}

// TagOf is the harness: tag of id: its adapter's, else "harness:<id>" for
// an id no longer registered, Claude Code's for "".
func TagOf(id string) string {
	if a, ok := ByID(id); ok {
		return a.Tag()
	}
	if id == "" {
		return Default().Tag()
	}
	return "harness:" + id
}

// Register adds a, after the adapters already there, and returns what
// removes it again (tests). It panics on an id already registered: two
// adapters cannot share an id.
func Register(a Adapter) (unregister func()) {
	mu.Lock()
	defer mu.Unlock()
	for _, b := range slices.Concat(adapters, known) {
		if b.ID() == a.ID() {
			panic("harness: " + a.ID() + " registered twice")
		}
	}
	adapters = append(adapters, a)
	return func() {
		mu.Lock()
		defer mu.Unlock()
		adapters = slices.DeleteFunc(adapters, func(b Adapter) bool { return b.ID() == a.ID() })
	}
}

type ctxKey struct{}

// WithAdapter is ctx for code that runs on behalf of a (a hook event).
func WithAdapter(ctx context.Context, a Adapter) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// FromContext is the adapter WithAdapter put in ctx, Default when none.
func FromContext(ctx context.Context) Adapter {
	if a, ok := ctx.Value(ctxKey{}).(Adapter); ok && a != nil {
		return a
	}
	return Default()
}
