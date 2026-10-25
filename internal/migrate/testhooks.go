//go:build testhooks

package migrate

import (
	"context"
	"errors"
	"os"
)

// TestHookID is the migration a testhooks build adds, for CI's update
// proof (decision 0067): it passes and is recorded, unless
// BACKSIGHT_TEST_FAIL_MIGRATION is set, when it fails, so that the update
// rolls back. Release builds have no testhooks tag, hence no such
// migration.
const TestHookID = "9000-test-hook"

func init() {
	registry = append(registry, Migration{ID: TestHookID, Summary: "CI test hook (testhooks builds only)", Run: func(context.Context, Env) error {
		if os.Getenv("BACKSIGHT_TEST_FAIL_MIGRATION") != "" {
			return errors.New("test hook: BACKSIGHT_TEST_FAIL_MIGRATION is set")
		}
		return nil
	}})
}
