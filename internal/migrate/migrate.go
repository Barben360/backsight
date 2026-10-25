// Package migrate holds backsight's client migrations (SPEC §11.7): numbered,
// idempotent steps that bring an install's local files to what a new
// binary expects. `update apply` runs the new binary's pending ones right
// after the shim switch; each is recorded in state.json
// (applied_migrations) once it passed, so it never runs twice.
//
// A migration may change only state.json and config.toml: update apply
// snapshots both and puts them back on a rollback. Files of the config
// editor (Claude Code's settings) are off limits until the rollback replays
// their pre-images (decision 0067 block 2 review), so Env carries no home
// directory. A migration must be safe to run again on files it already
// migrated, and on a fresh install's files, which need no migration: setup
// does not mark them applied.
package migrate

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// Env is what a migration works on: backsight's own directories, where
// it changes state.json and config.toml only.
type Env struct {
	Dirs paths.Dirs
}

// Migration is one numbered step: ID is "NNNN-slug", never reused.
type Migration struct {
	ID      string
	Summary string
	Run     func(ctx context.Context, env Env) error
}

// registry is append-only: a released migration is never removed,
// renumbered or edited (an install may already have recorded it).
var registry []Migration

// All is the registry of this binary, in order.
func All() []Migration { return slices.Clone(registry) }

var idRE = regexp.MustCompile(`^([0-9]{4})-[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Validate checks that ids are "NNNN-slug", strictly increasing by number,
// and that each migration has a function.
func Validate(list []Migration) error {
	last := -1
	for _, m := range list {
		g := idRE.FindStringSubmatch(m.ID)
		if g == nil {
			return fmt.Errorf("migration %q: the id must be NNNN-slug", m.ID)
		}
		n, _ := strconv.Atoi(g[1])
		if n <= last {
			return fmt.Errorf("migration %q: numbers must strictly increase", m.ID)
		}
		if m.Run == nil {
			return fmt.Errorf("migration %q has no function", m.ID)
		}
		last = n
	}
	return nil
}

// Pending is the part of list not recorded in applied, in order. Ids in
// applied this binary does not know (a newer binary's) are ignored.
func Pending(list []Migration, applied []string) []Migration {
	var out []Migration
	for _, m := range list {
		if !slices.Contains(applied, m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// Apply runs the pending migrations of list in order, recording each in
// state.json right after it passed. It stops at the first failure: what
// passed stays recorded, the failed one and those after it do not.
func Apply(ctx context.Context, env Env, list []Migration) (ran []string, err error) {
	if err := Validate(list); err != nil {
		return nil, err
	}
	st, err := state.Load(env.Dirs.StateFile())
	if err != nil {
		return nil, err
	}
	for _, m := range Pending(list, st.AppliedMigrations) {
		if err := ctx.Err(); err != nil {
			return ran, err
		}
		if err := m.Run(ctx, env); err != nil {
			return ran, fmt.Errorf("migration %s: %w", m.ID, err)
		}
		if err := state.Update(env.Dirs, "update migrate", func(s *state.State) error {
			if !slices.Contains(s.AppliedMigrations, m.ID) {
				s.AppliedMigrations = append(s.AppliedMigrations, m.ID)
			}
			return nil
		}); err != nil {
			return ran, fmt.Errorf("migration %s passed but could not be recorded: %w", m.ID, err)
		}
		ran = append(ran, m.ID)
	}
	return ran, nil
}
