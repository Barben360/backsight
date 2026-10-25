package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/target"
	"github.com/Barben360/backsight/internal/transcript"
)

// Memory noise (decision 0071 "Doctor", E30). Only the memory bank is read,
// never <bank>-control or <bank>-docs.
const (
	transverseNamedPct  = 20 // max % of transverse observations naming another project
	transverseMinN      = 20
	projectNameMinChars = 4 // shorter names match too much ordinary text
	headlessScan        = 1 << 20
)

// transverseReadMax bounds the observations read from the transverse pool;
// a variable so tests can shrink it.
var transverseReadMax = 2000

var errReadMax = errors.New("read bound reached")

// checkNoise runs the noise checks: the server-side ones when the bank
// check passed, else skipped like untagged; the local one always.
func checkNoise(ctx context.Context, env Env, cl *hindsight.Client, tg *target.Target, bank, srv Check) []Check {
	cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home)
	if err != nil {
		cfg = config.Default(env.Dirs, env.Home) // the reference install: backsight's defaults
	}
	var out []Check
	if bank.Status == OK || bank.Status == Warn {
		out = append(out, checkNoiseTransverse(ctx, cl, tg.Bank, cfg.Projects.Transverse))
		out = append(out, checkNoiseSubagent(ctx, cl, tg.Bank))
	} else {
		why := "skipped: " + skipReason(srv)
		out = append(out, Check{ID: "noise-transverse", Status: Info, Summary: why}, Check{ID: "noise-subagent", Status: Info, Summary: why})
	}
	return append(out, checkNoiseHeadless(env, cfg))
}

// checkNoiseTransverse: the transverse pool is searched with every project,
// so its observations about a named repository are noise everywhere else
// (E30 V3: about 40 %).
func checkNoiseTransverse(ctx context.Context, cl *hindsight.Client, bank string, transverse []string) Check {
	c := Check{ID: "noise-transverse"}
	alias := transverse[0]
	tags, err := cl.Tags(ctx, bank, "project:")
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list the bank's project tags", err.Error()
		return c
	}
	names := map[string]*regexp.Regexp{}
	for _, t := range tags {
		name := strings.TrimPrefix(t.Tag, "project:")
		if utf8.RuneCountInString(name) < projectNameMinChars || slices.Contains(transverse, name) {
			continue
		}
		names[name] = wordPattern(name)
	}
	total, named := 0, 0
	counts := map[string]int{}
	more := false
	err = cl.EachTagged(ctx, bank, "observation", "", "project:"+alias, func(m hindsight.Memory) error {
		if total == transverseReadMax {
			more = true
			return errReadMax
		}
		total++
		hit := false
		for name, re := range names {
			if re.MatchString(m.Text) {
				counts[name]++
				hit = true
			}
		}
		if hit {
			named++
		}
		return nil
	})
	if errors.Is(err, errReadMax) {
		err = nil
	}
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list the transverse observations", err.Error()
		return c
	}
	if total == 0 {
		c.Status, c.Summary, c.Projects = Info, "the transverse pool project:"+alias+" holds no observation", []string{alias}
		return c
	}
	c.Projects = append([]string{alias}, sortedKeys(counts)...)
	c.Summary = fmt.Sprintf("%d of %d transverse observations (project:%s) name another project", named, total, alias)
	if top := topCounts(counts, 3); top != "" {
		c.Summary += ": " + top
	}
	if more {
		c.Detail = fmt.Sprintf("only the first %d observations were read", transverseReadMax)
	}
	switch {
	case total < transverseMinN:
		c.Status, c.Summary = Info, c.Summary+"; too few to judge"
	case 100*named > transverseNamedPct*total:
		c.Status = Warn
		c.Detail = strings.TrimPrefix(c.Detail+"; the transverse pool is searched with every project (decision 0071, E30 V3)", "; ")
		c.Fix = "new sessions are attributed to their repository (decision 0071); clean older ones with backsight forget --project " + alias + " --source subagent --plan, or leave them"
	default:
		c.Status = OK
	}
	return c
}

// wordPattern matches name as a whole token, case-insensitively: a letter,
// digit, '-' or '_' next to it makes it part of another word (infra is
// not infrastructure, garage is not my-garage).
func wordPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_-])` + regexp.QuoteMeta(name) + `(?:$|[^\p{L}\p{N}_-])`)
}

// topCounts is the n largest counts, "name (count)", largest first.
func topCounts(counts map[string]int, n int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s (%d)", k, counts[k])
	}
	return strings.Join(parts, ", ")
}

// checkNoiseSubagent shows how much of what the bank learned comes from
// subagent reports (decision 0071 C4, C5); a share is not a fault.
func checkNoiseSubagent(ctx context.Context, cl *hindsight.Client, bank string) Check {
	c := Check{ID: "noise-subagent", Status: Info}
	tags, err := cl.Tags(ctx, bank, "source:")
	if err != nil {
		c.Summary, c.Detail = "cannot list the bank's source tags", err.Error()
		return c
	}
	n := map[string]int{}
	for _, t := range tags {
		n[strings.TrimPrefix(t.Tag, "source:")] += t.Count
	}
	total := n["chat"] + n["subagent"] + n["note"]
	if total == 0 {
		c.Summary = "no raw fact with a chat, subagent or note source yet"
		return c
	}
	c.Summary = fmt.Sprintf("subagent reports are %.0f%% of raw facts (%d of %d; chat %d, note %d)",
		100*float64(n["subagent"])/float64(total), n["subagent"], total, n["chat"], n["note"])
	return c
}

// checkNoiseHeadless finds the sessions this machine sent before headless
// entrypoints were skipped (decision 0071 C1): a cursor that sent records
// for a transcript whose entrypoint privacy.skip_entrypoints now matches.
func checkNoiseHeadless(env Env, cfg config.Config) Check {
	c := Check{ID: "noise-headless", Status: OK, Summary: "no headless session was sent"}
	dir := env.Dirs.CursorsDir()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	res := project.New(env.Home, cfg.Projects.Transverse)
	perProject := map[string]int{}
	sessions := 0
	for _, f := range files {
		cur, ok, err := retain.LoadCursor(dir, strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || !ok || cur.Records == 0 || (cur.Bank != "" && cur.Bank != cfg.Server.Bank) {
			continue
		}
		ep, cwd := headlessStart(cur.Transcript)
		if !cfg.Privacy.SkipsEntrypoint(ep) {
			continue
		}
		sessions++
		p := "(unknown project)"
		if cwd != "" {
			p = res.Project(cwd)
		}
		perProject[p]++
	}
	if sessions == 0 {
		return c
	}
	c.Status = Warn
	c.Summary = fmt.Sprintf("%d headless session(s) were sent before privacy.skip_entrypoints: %s", sessions, topCounts(perProject, len(perProject)))
	c.Detail = "check with --plan that a project holds nothing else worth keeping before forgetting it"
	var fixes, kept []string
	for _, p := range sortedKeys(perProject) {
		if p != "(unknown project)" {
			c.Projects = append(c.Projects, p)
		}
		switch {
		case slices.Contains(cfg.Projects.Transverse, p):
			kept = append(kept, p)
		case p != "(unknown project)":
			fixes = append(fixes, "backsight forget --project "+p+" --plan")
		}
	}
	if len(kept) > 0 {
		c.Detail += "; not " + strings.Join(kept, ", ") + ": the transverse pool also holds interactive sessions"
	}
	c.Fix = strings.Join(fixes, "; ")
	if c.Fix == "" {
		c.Fix = "leave them, or forget their memories one by one (backsight forget <id>)"
	}
	return c
}

// headlessStart reads a transcript's entrypoint and its first record's
// directory from its first MiB; empty when the file is gone.
func headlessStart(path string) (entrypoint, cwd string) {
	if path == "" {
		return "", ""
	}
	fh, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer func() { _ = fh.Close() }()
	if entrypoint = transcript.Entrypoint(fh); entrypoint == "" {
		return "", ""
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return entrypoint, ""
	}
	recs, _, _ := transcript.Read(io.LimitReader(fh, headlessScan), 0)
	for _, r := range recs {
		if r.CWD != "" {
			return entrypoint, r.CWD
		}
	}
	return entrypoint, ""
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
