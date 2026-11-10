package migrate

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
)

// ClientsTableID moves Claude Code's [client] table of config.toml to
// [clients.claude_code] (issue 277: one table per coding tool). The first
// shipped migration is numbered 0100, not 0001: update apply prints the
// ids, and a 00NN number reads as a design record's (publictext_test).
const ClientsTableID = "0100-clients-table"

// clientsTable renames the [client] table in place (config.MigrateClientTable),
// through the config editor: a backup copy of the file and a journal line
// first, written only over the bytes read, so `client config restore <run>`
// puts it back. The file is left as it is, and the migration passes, when
// there is nothing to move, when the file cannot be read as a config, or
// when the line edit cannot change it safely (a dotted key, an inline
// table, a multi-line string) or would change what it says: the older name
// is still read, and an update must not stop on it.
func clientsTable(_ context.Context, env Env) error {
	path := env.Dirs.ConfigFile()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, changed, err := config.MigrateClientTable(raw); err != nil || !changed {
		return nil
	}
	runner := claudecfg.Runner{PreImages: env.Dirs.PreImagesDir(), Journal: env.Dirs.JournalFile(), Command: "update migrate"}
	_, _, err = runner.Apply([]claudecfg.Edit{{Target: path, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, changed, err := config.MigrateClientTable(doc)
		if err != nil || !changed {
			return doc, nil, nil
		}
		before, err := config.Parse(doc, path, env.Dirs, "")
		if err != nil {
			return doc, nil, nil
		}
		after, err := config.Parse(out, path, env.Dirs, "")
		if err != nil || !config.SameValues(before, after) {
			return doc, nil, nil
		}
		from, to := "[client]", "[clients.claude_code]"
		return out, []claudecfg.Change{{Path: "client", Old: &from, New: &to}}, nil
	}}})
	return err
}
