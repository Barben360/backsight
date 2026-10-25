//go:build testhooks

package migrate

import (
	"context"
	"slices"
	"testing"
)

// CI runs this file with -tags testhooks: the hook migration is registered
// and fails on demand.
func TestTheTestHookMigration(t *testing.T) {
	if !slices.ContainsFunc(All(), func(m Migration) bool { return m.ID == TestHookID }) {
		t.Fatal("no test hook migration in a testhooks build")
	}
	d := dirs(t)
	if _, err := Apply(context.Background(), Env{Dirs: d}, All()); err != nil {
		t.Fatal(err)
	}
	d = dirs(t)
	t.Setenv("BACKSIGHT_TEST_FAIL_MIGRATION", "1")
	if _, err := Apply(context.Background(), Env{Dirs: d}, All()); err == nil {
		t.Fatal("the hook did not fail")
	}
	if slices.Contains(applied(t, d), TestHookID) {
		t.Error("a failed migration was recorded")
	}
}
