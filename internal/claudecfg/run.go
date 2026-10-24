package claudecfg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Edit is one Claude Code file and the operations backsight wants in it.
// Apply must be idempotent: it is re-run on a fresh read when the file
// changes between the read and the write.
type Edit struct {
	Target string
	Apply  func(doc []byte) ([]byte, []Change, error)
}

// FileResult is what a plan or a run did to one file.
type FileResult struct {
	Target  string   `json:"target"`
	Changes []Change `json:"changes"`
	Written bool     `json:"written"`
}

// Manifest describes one run's pre-images (decision 0052).
type Manifest struct {
	TS      string         `json:"ts"`
	Command string         `json:"command"`
	Files   []ManifestFile `json:"files"`
}

// ManifestFile: PreImage is nil when the file did not exist.
type ManifestFile struct {
	Target    string  `json:"target"`
	PreImage  *string `json:"pre_image"`
	ShaBefore string  `json:"sha_before"`
	ShaAfter  string  `json:"sha_after"`
}

// Runner writes edits with pre-images and a journal.
type Runner struct {
	PreImages string // <state>/pre-images
	Journal   string // <state>/journal.jsonl
	Command   string
	Now       func() time.Time
}

const (
	keepRuns    = 20
	maxAttempts = 3
)

// ErrBusy: a file kept changing under every attempt.
var ErrBusy = errors.New("kept changing while backsight edited it (Claude Code running?): try again")

// Plan reads each file and reports the changes without writing.
func Plan(edits []Edit) ([]FileResult, error) {
	var res []FileResult
	for _, e := range edits {
		doc, _, err := fsutil.ReadHashed(e.Target)
		if err != nil {
			return res, err
		}
		_, changes, err := e.Apply(doc)
		if err != nil {
			return res, fmt.Errorf("%s: %w", e.Target, err)
		}
		res = append(res, FileResult{Target: e.Target, Changes: changed(changes)})
	}
	return res, nil
}

// Apply writes every edit that changes its file. The first write creates the
// run's pre-image directory; ts is empty when nothing was written.
func (r Runner) Apply(edits []Edit) (string, []FileResult, error) {
	run := runState{r: r, action: "edit"}
	var res []FileResult
	for _, e := range edits {
		fr, err := run.edit(e)
		res = append(res, fr)
		if err != nil {
			return run.ts, res, err
		}
	}
	return run.ts, res, nil
}

type runState struct {
	r      Runner
	ts     string
	m      Manifest
	action string // journal action: edit | restore
}

func (s *runState) edit(e Edit) (FileResult, error) {
	fr := FileResult{Target: e.Target}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		doc, sha, err := fsutil.ReadHashed(e.Target)
		if err != nil {
			return fr, err
		}
		out, changes, err := e.Apply(doc)
		if err != nil {
			return fr, fmt.Errorf("%s: %w", e.Target, err)
		}
		fr.Changes = changed(changes)
		if len(fr.Changes) == 0 || string(out) == string(doc) {
			fr.Changes = nil
			return fr, nil
		}
		err = s.write(e.Target, doc, sha, out)
		if errors.Is(err, fsutil.ErrConflict) {
			continue
		}
		if err != nil {
			return fr, err
		}
		fr.Written = true
		return fr, nil
	}
	return fr, fmt.Errorf("%s %w", e.Target, ErrBusy)
}

// write saves the pre-image of exactly the bytes being replaced, then the
// file if it is unchanged since read (after nil: deletes it), then the
// manifest and journal.
func (s *runState) write(target string, before []byte, sha string, after []byte) error {
	if s.ts == "" {
		ts, err := s.r.newRun()
		if err != nil {
			return err
		}
		s.ts, s.m = ts, Manifest{TS: ts, Command: s.r.Command}
	}
	dir := filepath.Join(s.r.PreImages, s.ts)
	mf := ManifestFile{Target: target, ShaBefore: sha, ShaAfter: hash(after)}
	if after == nil {
		mf.ShaAfter = fsutil.Absent
	}
	if sha != fsutil.Absent {
		name := fmt.Sprintf("%02d-%s", len(s.m.Files)+1, filepath.Base(target))
		if err := fsutil.WriteAtomic(filepath.Join(dir, name), before, 0o600); err != nil {
			return err
		}
		mf.PreImage = &name
	}
	if after == nil {
		if err := removeIfUnchanged(target, sha); err != nil {
			return err
		}
	} else if err := fsutil.ReplaceIfUnchanged(target, after, 0o600, sha); err != nil {
		return err
	}
	s.m.Files = append(s.m.Files, mf)
	if err := writeManifest(dir, s.m); err != nil {
		return err
	}
	return s.r.journal(s.ts, s.action, target, mf.PreImage)
}

// removeIfUnchanged deletes path only if its hash is still want.
func removeIfUnchanged(path, want string) error {
	got, err := fsutil.HashFile(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: %w", path, fsutil.ErrConflict)
	}
	return os.Remove(path)
}

// newRun creates a fresh pre-image directory and prunes old runs.
func (r Runner) newRun() (string, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	base := now().UTC().Format("20060102T150405.000000Z")
	ts := base
	for i := 2; ; i++ {
		err := os.MkdirAll(r.PreImages, 0o700)
		if err == nil {
			err = os.Mkdir(filepath.Join(r.PreImages, ts), 0o700)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		ts = fmt.Sprintf("%s-%d", base, i)
	}
	runs, _ := Runs(r.PreImages)
	for len(runs) > keepRuns {
		_ = os.RemoveAll(filepath.Join(r.PreImages, runs[0]))
		runs = runs[1:]
	}
	return ts, nil
}

// Runs lists the run directories, oldest first.
func Runs(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []string
	for _, e := range ents {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	sort.Strings(runs)
	return runs, nil
}

// LoadManifest reads one run's manifest.
func LoadManifest(dir, ts string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ts, "manifest.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func writeManifest(dir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o600)
}

// RestoreResult is one file a restore put back, or refused.
type RestoreResult struct {
	Target string `json:"target"`
	Status string `json:"status"` // restored | deleted | unchanged | refused
	Reason string `json:"reason,omitempty"`
}

// ErrChangedSince: a file was edited after the run; --force overrides.
var ErrChangedSince = errors.New("changed since that run: --force restores it anyway")

// Restore puts back the pre-images of run ts (the last run when empty). It
// is itself a run, so it can be undone. Files changed since are refused
// unless force; then nothing is written at all.
func (r Runner) Restore(ts string, force bool) (string, []RestoreResult, error) {
	if ts == "" {
		runs, err := Runs(r.PreImages)
		if err != nil {
			return "", nil, err
		}
		if len(runs) == 0 {
			return "", nil, errors.New("no config editor run to restore")
		}
		ts = runs[len(runs)-1]
	}
	m, err := LoadManifest(r.PreImages, ts)
	if err != nil {
		return "", nil, fmt.Errorf("run %s: %w", ts, err)
	}
	var res []RestoreResult
	refused := false
	for _, f := range m.Files {
		cur, err := fsutil.HashFile(f.Target)
		if err != nil {
			return "", nil, err
		}
		rr := RestoreResult{Target: f.Target}
		switch {
		case cur == f.ShaBefore:
			rr.Status = "unchanged"
		case cur != f.ShaAfter && !force:
			rr.Status, rr.Reason, refused = "refused", ErrChangedSince.Error(), true
		}
		res = append(res, rr)
	}
	if refused {
		return ts, res, fmt.Errorf("%d file(s) %w", countStatus(res, "refused"), ErrChangedSince)
	}
	run := runState{r: r, action: "restore"}
	for i, f := range m.Files {
		if res[i].Status == "unchanged" {
			continue
		}
		var want []byte // nil: the run created the file, so restoring deletes it
		if f.PreImage != nil {
			if want, err = os.ReadFile(filepath.Join(r.PreImages, ts, *f.PreImage)); err != nil {
				return ts, res, err
			}
			if want == nil {
				want = []byte{}
			}
		}
		if err := run.put(f.Target, want); err != nil {
			return run.ts, res, err
		}
		res[i].Status = map[bool]string{true: "deleted", false: "restored"}[want == nil]
	}
	return run.ts, res, nil
}

// put makes target hold want (nil: absent), backing up what it replaces;
// a concurrent change is retried like an edit.
func (s *runState) put(target string, want []byte) error {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		doc, sha, err := fsutil.ReadHashed(target)
		if err != nil {
			return err
		}
		err = s.write(target, doc, sha, want)
		if !errors.Is(err, fsutil.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("%s %w", target, ErrBusy)
}

func (r Runner) journal(ts, action, target string, pre *string) error {
	line := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "command": r.Command, "step": "claude-config",
		"action": action, "target": target, "pre_image": nil, "status": "done", "run": ts}
	if pre != nil {
		line["pre_image"] = filepath.Join(r.PreImages, ts, *pre)
	}
	b, _ := json.Marshal(line)
	if err := os.MkdirAll(filepath.Dir(r.Journal), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.Journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func changed(cs []Change) []Change {
	var out []Change
	for _, c := range cs {
		if c.Changed() {
			out = append(out, c)
		}
	}
	return out
}

func countStatus(rs []RestoreResult, s string) int {
	n := 0
	for _, r := range rs {
		if r.Status == s {
			n++
		}
	}
	return n
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
