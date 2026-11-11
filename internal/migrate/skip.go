package migrate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
)

// HeadlessSkipID brings privacy.skip_entrypoints from its former default,
// ["sdk-*"], to today's, which also skips Codex's headless runs (codex
// exec, its SDKs, Codex as another agent's tool; block 6 of the Codex
// epic, https://github.com/Barben360/backsight/issues/281). Setup wrote the
// whole default into every config.toml, so an existing install would
// otherwise never get it.
const HeadlessSkipID = "0101-headless-skip-entrypoints"

// headlessSkip sets privacy.skip_entrypoints to today's default only when
// config.toml sets exactly ["sdk-*"] (config.MigrateSkipEntrypoints),
// through the config editor: a backup copy and a journal line first,
// written only over the bytes read, so `client config restore <run>` puts
// the line back. Any other value, a file without the key, a file that
// does not read as a config, and a form a line edit cannot change are
// left as they are, and the migration passes: the value the user has
// still holds, and an update must not stop on it.
func headlessSkip(_ context.Context, env Env) error {
	path := env.Dirs.ConfigFile()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, changed, _ := config.MigrateSkipEntrypoints(raw); !changed {
		return nil
	}
	runner := claudecfg.Runner{PreImages: env.Dirs.PreImagesDir(), Journal: env.Dirs.JournalFile(), Command: "update migrate"}
	_, _, err = runner.Apply([]claudecfg.Edit{{Target: path, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, changed, _ := config.MigrateSkipEntrypoints(doc)
		if !changed {
			return doc, nil, nil
		}
		before, err := config.Parse(doc, path, env.Dirs, "")
		if err != nil {
			return doc, nil, nil
		}
		after, err := config.Parse(out, path, env.Dirs, "")
		if err != nil || !slices.Equal(after.Privacy.SkipEntrypoints, config.DefaultSkipEntrypoints()) {
			return doc, nil, nil
		}
		before.Privacy.SkipEntrypoints = after.Privacy.SkipEntrypoints
		if !config.SameValues(before, after) {
			return doc, nil, nil // the edit would change something else
		}
		from, to := `["sdk-*"]`, `["`+strings.Join(config.DefaultSkipEntrypoints(), `", "`)+`"]`
		return out, []claudecfg.Change{{Path: "privacy.skip_entrypoints", Old: &from, New: &to}}, nil
	}}})
	return err
}
