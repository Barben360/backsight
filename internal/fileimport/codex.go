package fileimport

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The Codex preset (`backsight import --from codex`, decision 0085 D10,
// amending 0081): Codex's own memories, read from the stores of a Codex
// home. The files are Codex's generated state, written by a model with no
// format guarantee: the reader keeps to the names and headers the spike
// recorded (research C14 §11), skips anything else with a reason, and
// never fails on a file's content.

// PresetCodex names the Codex preset in a manifest and a plan.
const PresetCodex = "codex"

// CodexStores are the stores a Codex home may hold, in the order they are
// preferred when both hold the same file (dual write): v2 is the default.
var CodexStores = []string{"memories_v2", "memories"}

// Skip reasons of the Codex preset.
const (
	SkipCodexWorking  = "codex_working"   // Codex's own working files: raw_memories.md, phase2_workspace_diff.md, extensions/*/instructions.md
	SkipCodexSkills   = "codex_skills"    // skills/: instructions Codex wrote for itself, not memories
	SkipCodexNotes    = "codex_notes"     // extensions/*/resources/*.md: never seen written yet, not read until a real store shows one
	SkipCodexUnknown  = "codex_unknown"   // a name the layout does not have
	SkipRolloutOnDisk = "rollout_on_disk" // a summary whose conversation is still on disk: past sessions send it
	SkipBadHeader     = "bad_header"      // a summary whose header cannot be read
	SkipOptedOut      = "opted_out"       // a summary of a folder kept out of memory (privacy.exclude_paths, .backsight.toml)
	SkipMixesExcluded = "mixes_excluded"  // a consolidated file of a store that holds a kept-out folder's work
	SkipDuplicate     = "duplicate"       // the same file, or thread, in the other store
)

// Bounds on what is read from a Codex home beyond the files themselves.
const (
	codexHeaderBytes   = 4096    // a summary's header is read in its first 4 KiB
	codexHeaderLines   = 16      // and in at most 16 lines
	codexSessionsLimit = 200_000 // entries of sessions/ looked at for rollouts
)

// CodexOptions tunes a Codex scan. Project names the project of a
// working folder and OptedOut tells whether one is kept out of memory,
// both as for a session (project.Resolver); nil OptedOut keeps none out.
type CodexOptions struct {
	Exclude  []string // globs relative to the Codex home
	Now      time.Time
	Project  func(cwd string) string
	OptedOut func(cwd string) bool
}

// CodexSummary is the header of a rollout summary.
type CodexSummary struct {
	ThreadID    string
	UpdatedAt   time.Time
	RolloutPath string
	CWD         string
}

// ParseCodexSummary reads the header Codex writes at the top of a
// rollout summary (thread_id, updated_at, rollout_path, cwd, git_branch
// lines, then a blank line); false when one of the four it needs is
// missing or malformed.
func ParseCodexSummary(raw []byte) (CodexSummary, bool) {
	head := raw[:min(len(raw), codexHeaderBytes)]
	var s CodexSummary
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(head))
	for n := 0; sc.Scan() && n < codexHeaderLines; n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if seen[key] {
			return CodexSummary{}, false // two values: which one is right is not ours to guess
		}
		seen[key] = true
		switch key {
		case "thread_id":
			s.ThreadID = val
		case "updated_at":
			t, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return CodexSummary{}, false
			}
			s.UpdatedAt = t.UTC()
		case "rollout_path":
			s.RolloutPath = val
		case "cwd":
			s.CWD = val
		}
	}
	if !validThreadID(s.ThreadID) || s.UpdatedAt.IsZero() || !localAbs(s.RolloutPath) || !localAbs(s.CWD) {
		return CodexSummary{}, false
	}
	return s, true
}

// validThreadID: what Codex writes, a UUID; letters, digits and dashes,
// at most 64.
func validThreadID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
		default:
			return false
		}
	}
	return true
}

// localAbs: an absolute path on this machine's disks, or a rooted one
// (/home/u/src: on Windows, the current drive's). A network path
// (\\host\share, //host/share) is refused: looking at it would reach
// another machine.
func localAbs(p string) bool {
	return p != "" && len(p) <= 4096 && (filepath.IsAbs(p) || strings.HasPrefix(p, "/")) && !strings.ContainsRune(p, 0) &&
		!strings.HasPrefix(p, `\\`) && !strings.HasPrefix(p, "//")
}

// codexSkip is the Codex layout, by path relative to a store.
func codexSkip(rel string, dir bool) string {
	parts := strings.Split(rel, "/")
	if dir {
		switch {
		case rel == "skills":
			return SkipCodexSkills
		case rel == "rollout_summaries", rel == "extensions",
			len(parts) == 2 && parts[0] == "extensions",
			len(parts) == 3 && parts[0] == "extensions" && parts[2] == "resources":
			return ""
		}
		return SkipCodexUnknown
	}
	switch {
	case rel == "memory_summary.md", rel == "MEMORY.md":
		return ""
	case rel == "raw_memories.md", rel == "phase2_workspace_diff.md":
		return SkipCodexWorking
	case len(parts) == 2 && parts[0] == "rollout_summaries" && strings.HasSuffix(parts[1], ".md"):
		return ""
	case len(parts) == 3 && parts[0] == "extensions" && parts[2] == "instructions.md":
		return SkipCodexWorking
	case len(parts) == 4 && parts[0] == "extensions" && parts[2] == "resources":
		return SkipCodexNotes
	}
	return SkipCodexUnknown
}

// isCodexSummary reports whether rel (relative to the Codex home) is a
// rollout summary of a store.
func isCodexSummary(rel string) bool {
	parts := strings.Split(rel, "/")
	return len(parts) == 3 && slices.Contains(CodexStores, parts[0]) && parts[1] == "rollout_summaries"
}

// isCodexConsolidated reports whether rel (relative to the Codex home) is
// a store's consolidated file.
func isCodexConsolidated(rel string) bool {
	store, name, ok := strings.Cut(rel, "/")
	return ok && slices.Contains(CodexStores, store) && (name == "memory_summary.md" || name == "MEMORY.md")
}

// ScanCodex reads the memory stores of the Codex home home (every one
// present: memories_v2/, memories/, both under dual write), as an import
// of root home with paths relative to it:
//   - memory_summary.md and MEMORY.md go to the import's project, dated by
//     their header or modification time (0081's rule); not when the
//     store also holds a kept-out folder's work, which they may quote;
//   - a rollout_summaries/*.md goes to the project of its header's cwd,
//     dated by its updated_at, only when its conversation is no longer on
//     disk (past sessions send the ones that are) and its folder is not
//     kept out;
//   - the same file, or the same thread's summary, in both stores goes
//     once;
//   - everything else is skipped with its reason.
func ScanCodex(home string, opt CodexOptions) (Scan, error) {
	abs, err := ResolveRoot(home)
	if err != nil {
		return Scan{}, err
	}
	if opt.Project == nil {
		opt.Project = filepath.Base
	}
	optedOut := func(cwd string) bool { return opt.OptedOut != nil && opt.OptedOut(cwd) }
	out := Scan{Root: abs, Preset: PresetCodex, Skipped: map[string][]string{}, SkippedDirs: map[string][]string{}}
	threads := codexRollouts(filepath.Join(abs, "sessions"))
	summaries := map[string]CodexSummary{} // rel -> header, of the summaries to send
	keptOut := map[string]bool{}           // store -> holds a kept-out folder's work
	for _, store := range CodexStores {
		dir := filepath.Join(abs, store)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			out.SkippedDirs[SkipUnreadable] = append(out.SkippedDirs[SkipUnreadable], store)
			continue
		case fi.Mode()&fs.ModeSymlink != 0:
			out.SkippedDirs[SkipSymlink] = append(out.SkippedDirs[SkipSymlink], store)
			continue
		case !fi.IsDir():
			out.Skipped[SkipCodexUnknown] = append(out.Skipped[SkipCodexUnknown], store)
			continue
		}
		out.Stores = append(out.Stores, store)
		inspect := func(rel string, raw []byte, f *File) string {
			if !isCodexSummary(store + "/" + rel) {
				return ""
			}
			sum, ok := ParseCodexSummary(raw)
			if !ok {
				return SkipBadHeader
			}
			if optedOut(sum.CWD) {
				keptOut[store] = true
				return SkipOptedOut
			}
			if rolloutOnDisk(sum, threads) {
				return SkipRolloutOnDisk
			}
			f.Date, f.DateFromMtime, f.Project = sum.UpdatedAt, false, opt.Project(sum.CWD)
			summaries[store+"/"+rel] = sum
			return ""
		}
		s, err := ScanDir(dir, ScanOptions{Now: opt.Now, Skip: codexSkip, Inspect: inspect})
		if err != nil {
			out.SkippedDirs[SkipUnreadable] = append(out.SkippedDirs[SkipUnreadable], store)
			continue
		}
		for _, f := range s.Files {
			f.Rel = store + "/" + f.Rel
			out.Files = append(out.Files, f)
		}
		for _, m := range []struct{ from, to map[string][]string }{{s.Skipped, out.Skipped}, {s.SkippedDirs, out.SkippedDirs}} {
			for reason, rels := range m.from {
				for _, r := range rels {
					m.to[reason] = append(m.to[reason], store+"/"+r)
				}
			}
		}
		if !keptOut[store] && rawMemoriesKeepOut(filepath.Join(dir, "raw_memories.md"), optedOut) {
			keptOut[store] = true
		}
	}
	keep := out.Files[:0]
	for _, f := range out.Files {
		store, _, _ := strings.Cut(f.Rel, "/")
		reason := ""
		switch {
		case slices.ContainsFunc(opt.Exclude, func(g string) bool { return MatchGlob(g, f.Rel) }):
			reason = SkipExcluded
		case isCodexConsolidated(f.Rel) && keptOut[store]:
			reason = SkipMixesExcluded
		}
		if reason != "" {
			out.Skipped[reason] = append(out.Skipped[reason], f.Rel)
			continue
		}
		keep = append(keep, f)
	}
	out.Files = dedupeCodex(keep, summaries, &out)
	slices.SortFunc(out.Files, func(a, b File) int { return strings.Compare(a.Rel, b.Rel) })
	for _, m := range []map[string][]string{out.Skipped, out.SkippedDirs} {
		for _, v := range m {
			slices.Sort(v)
		}
	}
	return out, nil
}

// dedupeCodex keeps one copy of what both stores hold: a file with the
// same content, or the summary of the same thread (the newer one), the
// first store of CodexStores winning a tie.
func dedupeCodex(files []File, summaries map[string]CodexSummary, out *Scan) []File {
	rank := func(rel string) int {
		store, _, _ := strings.Cut(rel, "/")
		return slices.Index(CodexStores, store)
	}
	slices.SortStableFunc(files, func(a, b File) int { return rank(a.Rel) - rank(b.Rel) })
	bySHA := map[string]bool{}
	byThread := map[string]int{} // thread -> index in kept
	var kept []File
	for _, f := range files {
		if bySHA[f.SHA256] {
			out.Skipped[SkipDuplicate] = append(out.Skipped[SkipDuplicate], f.Rel)
			continue
		}
		if sum, ok := summaries[f.Rel]; ok {
			if i, dup := byThread[sum.ThreadID]; dup {
				if sum.UpdatedAt.After(summaries[kept[i].Rel].UpdatedAt) {
					out.Skipped[SkipDuplicate] = append(out.Skipped[SkipDuplicate], kept[i].Rel)
					kept[i] = f
					bySHA[f.SHA256] = true
				} else {
					out.Skipped[SkipDuplicate] = append(out.Skipped[SkipDuplicate], f.Rel)
				}
				continue
			}
			byThread[sum.ThreadID] = len(kept)
		}
		bySHA[f.SHA256] = true
		kept = append(kept, f)
	}
	return kept
}

// rolloutOnDisk reports whether a summary's conversation is still on
// disk: at its rollout_path (compressed in place too), or under the
// home's sessions/ by its thread id (a home that moved).
func rolloutOnDisk(sum CodexSummary, threads map[string]bool) bool {
	for _, p := range []string{sum.RolloutPath, sum.RolloutPath + ".zst"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return threads[strings.ToLower(sum.ThreadID)]
}

// codexRollouts are the thread ids of the rollouts under sessions/
// (rollout-<time>-<thread id>.jsonl, or .jsonl.zst), links not followed,
// at most codexSessionsLimit entries looked at.
func codexRollouts(sessions string) map[string]bool {
	ids := map[string]bool{}
	n := 0
	_ = filepath.WalkDir(sessions, func(p string, d fs.DirEntry, err error) error {
		if n++; n > codexSessionsLimit {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		base, ok := strings.CutSuffix(name, ".jsonl")
		if !ok {
			base, ok = strings.CutSuffix(name, ".jsonl.zst")
		}
		const uuidLen = 36
		if !ok || !strings.HasPrefix(base, "rollout-") || len(base) < len("rollout-")+uuidLen+1 {
			return nil
		}
		if id := base[len(base)-uuidLen:]; base[len(base)-uuidLen-1] == '-' && validThreadID(id) {
			ids[strings.ToLower(id)] = true
		}
		return nil
	})
	return ids
}

// rawMemoriesKeepOut reports whether a v1 store's raw_memories.md (Codex's
// merge of every thread it remembers, each with its cwd) names a folder
// kept out of memory: the consolidated files are drawn from the same
// threads. Unreadable or too large: no.
func rawMemoriesKeepOut(p string, optedOut func(string) bool) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileSize {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(io.LimitReader(f, maxFileSize))
	sc.Buffer(make([]byte, 0, 64*1024), maxFileSize)
	for sc.Scan() {
		if cwd, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "cwd:"); ok {
			if cwd = strings.TrimSpace(cwd); localAbs(cwd) && optedOut(cwd) {
				return true
			}
		}
	}
	return false
}

// CodexRecheck is what a queued file of the Codex preset is worth when it
// is sent (rel relative to the Codex home, raw its bytes then): a summary
// is dated by its header again (dated true), and no longer sent when the
// header became unreadable or its folder was kept out since (reason).
func CodexRecheck(rel string, raw []byte, optedOut func(cwd string) bool) (date time.Time, dated bool, reason string) {
	if !isCodexSummary(rel) {
		return time.Time{}, false, ""
	}
	sum, ok := ParseCodexSummary(raw)
	switch {
	case !ok:
		return time.Time{}, false, SkipBadHeader
	case optedOut != nil && optedOut(sum.CWD):
		return time.Time{}, false, SkipOptedOut
	}
	return sum.UpdatedAt, true, ""
}
