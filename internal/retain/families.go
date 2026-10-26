package retain

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Project families (decision 0072): two projects worked on together, often
// and in several sessions, are searched together. They come from the
// projects each recorded turn touched (its own repositories, explicit or
// implicit evidence), never from a repository's files, so a cloned
// repository cannot choose them.
const (
	FamilyWindow      = 60 * 24 * time.Hour
	FamilyMinTurns    = 5   // turns touching both
	FamilyMinSessions = 2   // in at least this many sessions
	FamilyMinShare    = 0.2 // of the larger project's turns
	MaxFamily         = 10  // linked projects per project
)

// Link is one pair of projects worked on together.
type Link struct {
	A        string  `json:"a"`
	B        string  `json:"b"`
	Turns    int     `json:"turns"`    // turns touching both
	Sessions int     `json:"sessions"` // sessions those turns belong to
	Share    float64 `json:"share"`    // Turns / the larger project's turns
}

// Families is the families file: each project's linked projects, most
// co-touched first, and the links behind them.
type Families struct {
	BuiltAt time.Time           `json:"built_at"`
	Since   time.Time           `json:"since"`
	Turns   int                 `json:"turns"` // turns read
	Links   []Link              `json:"links"`
	Of      map[string][]string `json:"of"`
	// Roots: each member's main checkout, so recall can leave out one
	// kept out of memory since the file was built.
	Roots map[string]string `json:"roots,omitempty"`
}

// ComputeFamilies links the projects of turns: a pair is linked when at
// least FamilyMinTurns turns of at least FamilyMinSessions sessions touch
// both, and those turns are at least FamilyMinShare of the larger
// project's turns (so a project touched everywhere, a hub, does not link
// to every small one). Links are pairwise, never transitive; each project
// keeps its MaxFamily most co-touched ones.
//
// Subagent reports count as turns, as in E32: a main turn whose subagents
// work in both projects adds one co-touched turn per report. keep, when
// not nil, says whether a project may still be a member (not excluded,
// paused or forgotten since its turns were recorded).
func ComputeFamilies(turns []TurnEntry, keep func(project, root string) bool) Families {
	roots := map[string]string{}
	for _, t := range turns {
		for p, r := range t.Votes.Roots {
			if _, ok := roots[p]; !ok {
				roots[p] = r
			}
		}
	}
	allowed := map[string]bool{}
	ok := func(p string) bool {
		v, seen := allowed[p]
		if !seen {
			v = keep == nil || keep(p, roots[p])
			allowed[p] = v
		}
		return v
	}
	own := map[string]int{}
	type pair struct{ a, b string }
	co := map[pair]int{}
	sess := map[pair]map[string]bool{}
	for _, t := range turns {
		ps := slices.DeleteFunc(t.Votes.Projects(), func(p string) bool { return !ok(p) })
		slices.Sort(ps)
		ps = slices.Compact(ps)
		for _, p := range ps {
			own[p]++
		}
		for i := range ps {
			for j := i + 1; j < len(ps); j++ {
				k := pair{ps[i], ps[j]}
				co[k]++
				if sess[k] == nil {
					sess[k] = map[string]bool{}
				}
				sess[k][t.Session] = true
			}
		}
	}
	f := Families{Turns: len(turns), Of: map[string][]string{}, Roots: map[string]string{}}
	for k, n := range co {
		larger := max(own[k.a], own[k.b])
		share := float64(n) / float64(larger)
		if n >= FamilyMinTurns && len(sess[k]) >= FamilyMinSessions && share >= FamilyMinShare {
			f.Links = append(f.Links, Link{A: k.a, B: k.b, Turns: n, Sessions: len(sess[k]), Share: share})
		}
	}
	sort.Slice(f.Links, func(i, j int) bool {
		x, y := f.Links[i], f.Links[j]
		if x.Turns != y.Turns {
			return x.Turns > y.Turns
		}
		if x.A != y.A {
			return x.A < y.A
		}
		return x.B < y.B
	})
	for _, l := range f.Links {
		for _, e := range [][2]string{{l.A, l.B}, {l.B, l.A}} {
			if len(f.Of[e[0]]) < MaxFamily {
				f.Of[e[0]] = append(f.Of[e[0]], e[1])
				if r := roots[e[1]]; r != "" {
					f.Roots[e[1]] = r
				}
			}
		}
	}
	return f
}

// RefreshFamilies computes the families of the turns recorded over the
// last FamilyWindow and saves them in path. A project kept out of memory
// now (its checkout excluded, retain = false, paused) is no member;
// known, when not nil, says which projects the bank still holds (one
// forgotten since is no member either).
func RefreshFamilies(a Attributor, path string, now time.Time, known func(string) bool) (Families, error) {
	since := now.Add(-FamilyWindow)
	turns, err := a.Turns(since)
	if err != nil {
		return Families{}, err
	}
	f := ComputeFamilies(turns, func(p, root string) bool {
		if known != nil && !known(p) {
			return false
		}
		if root != "" && a.Resolver != nil {
			if out, _ := a.Resolver.OptedOut(root, a.Exclude); out {
				return false
			}
		}
		return true
	})
	f.BuiltAt, f.Since = now.UTC(), since.UTC()
	raw, err := json.Marshal(f)
	if err != nil {
		return f, err
	}
	return f, fsutil.WriteAtomic(path, raw, 0o600)
}

// LoadFamilies reads the families file; none is an empty one.
func LoadFamilies(path string) (Families, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Families{Of: map[string][]string{}}, nil
	}
	if err != nil {
		return Families{}, err
	}
	var f Families
	if err := json.Unmarshal(raw, &f); err != nil {
		return Families{}, err
	}
	if f.Of == nil {
		f.Of = map[string][]string{}
	}
	return f, nil
}
