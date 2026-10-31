package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/server"
)

// The native runtime's part of backup verify and restore (decision 0078,
// rulings R6, R36): a stray postgres stopped first, and a restore that
// adopts the archive into a new database when the old one is refused.

// stopNativeStrays stops, on native, each postgres running on the
// database's data outside its unit (requirement 3: never adopted), and
// says which; nothing on Docker. A stray StopStrays will not stop (its
// pid is not the one postmaster.pid names, ruling R36) is an error.
func stopNativeStrays(ctx context.Context, rt server.Runtime) ([]string, error) {
	n, ok := rt.(server.Native)
	if !ok {
		return nil, nil
	}
	strays, err := n.Strays()
	if err != nil {
		return nil, fmt.Errorf("looking for a postgres on %s outside %s: %w", n.DataDir(), n.DBOwner(), err)
	}
	if err := n.StopStrays(ctx, strays); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range strays {
		out = append(out, fmt.Sprintf("a postgres (pid %d) ran on %s outside %s: stopped, never adopted", s.PID, n.DataDir(), n.DBOwner()))
	}
	return out, nil
}

// nativeRefusal is, on native, why the database would not start: the
// identity check's refusal (missing, empty, another identity, unrecorded),
// or nothing at all there (no database and no identity: Start refuses it
// as missing, ruling R35). nil when it would start, and on Docker.
func nativeRefusal(ctx context.Context, rt server.Runtime) (*server.GuardError, error) {
	n, ok := rt.(server.Native)
	if !ok {
		return nil, nil
	}
	err := n.CheckInstance(ctx)
	var g *server.GuardError
	if errors.As(err, &g) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(n.InstanceDir()); errors.Is(err, fs.ErrNotExist) {
		return &server.GuardError{Reason: "missing", Instance: n.InstanceDir(), File: n.IdentityFile()}, nil
	} else if err != nil {
		return nil, err
	}
	return nil, nil
}

// refusalChanged says how the native refusal changed between before and
// now, "" when it is the same (none both times, or the same reason on the
// same database).
func refusalChanged(before, now *server.GuardError) string {
	switch {
	case before == nil && now == nil:
		return ""
	case before == nil:
		return "it is refused now: " + now.Reason
	case now == nil:
		return "it is no longer refused, it was " + before.Reason
	case before.Reason != now.Reason || before.Instance != now.Instance || before.Want != now.Want || before.Got != now.Got:
		return "it was refused as " + before.Reason + ", now as " + now.Reason
	}
	return ""
}

// refusedWarning says why the native database is refused and what restore
// does about it.
func refusedWarning(g *server.GuardError) string {
	why := g.Reason
	switch g.Reason {
	case "identity":
		why = "another database: identifier " + g.Got + ", recorded " + g.Want
	case "unrecorded":
		why = "no recorded identity"
	}
	if _, err := os.Lstat(g.Instance); err != nil {
		return "the native database " + g.Instance + " is refused (" + why + "): restore restores the archive into a new database"
	}
	return "the native database " + g.Instance + " is refused (" + why + "): restore moves it aside, kept, and restores the archive into a new database"
}

// refusedName is a refused database's name once moved aside, and its
// identity file's: <instance>.refused-<Unix seconds> (ruling R6).
func refusedName(path string, at time.Time) string {
	return path + ".refused-" + strconv.FormatInt(at.Unix(), 10)
}

// moveAsideRefused stops the native units (nothing may run on the refused
// database) and moves the refused database and its identity file aside,
// never deleting them: doctor lists them. moved is where the database
// went ("" when there was none). On an error nothing was moved.
func moveAsideRefused(ctx context.Context, rt server.Runtime, at time.Time) (moved string, err error) {
	n := rt.(server.Native)
	if err := n.Stop(ctx); err != nil {
		return "", err
	}
	inst, id := n.InstanceDir(), n.IdentityFile()
	for _, p := range []string{refusedName(inst, at), refusedName(id, at)} {
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("%s exists already", p)
		}
	}
	if _, err := os.Lstat(inst); err == nil {
		moved = refusedName(inst, at)
		if err := os.Rename(inst, moved); err != nil {
			return "", err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(id, refusedName(id, at)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if moved != "" {
			if back := os.Rename(moved, inst); back != nil {
				return moved, fmt.Errorf("%w; and %s could not be put back: %w", err, moved, back)
			}
		}
		return "", err
	}
	return moved, nil
}

// startFreshDatabase creates a new native database as a first install
// does (ruling R35's MayCreate), records its identity and starts it alone:
// the archive is restored into it.
func startFreshDatabase(ctx context.Context, rt server.Runtime, sec config.Secrets) error {
	n := rt.(server.Native)
	n.MayCreate = true
	files, err := renderOn(n, sec, "backsight restore")
	if err != nil {
		return err
	}
	if _, err := n.Write(files); err != nil {
		return err
	}
	return n.StartDatabase(ctx)
}

// removeFreshDatabase undoes startFreshDatabase after a failed restore:
// the units stopped, the new database and its identity removed, so that no
// start ever serves it (an empty or half-restored memory) and restore
// adopts the archive again. The refused database stays where it was moved.
func removeFreshDatabase(ctx context.Context, rt server.Runtime) error {
	n := rt.(server.Native)
	err := n.Stop(ctx)
	if rmErr := os.RemoveAll(n.InstanceDir()); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if rmErr := os.Remove(n.IdentityFile()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	return err
}

// recordNativeIdentity records, on native, the identity of the database
// the archive was restored into.
func recordNativeIdentity(ctx context.Context, rt server.Runtime) error {
	if n, ok := rt.(server.Native); ok {
		return n.RecordIdentity(ctx)
	}
	return nil
}
