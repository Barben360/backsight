package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
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
	headlessDocsMax     = 100 // headless documents checked for a live fact
	headlessFixesMax    = 10
	unknownProject      = "(unknown project)"
)

// transverseReadMax bounds the observations read from the transverse pool;
// a variable so tests can shrink it.
var transverseReadMax = 2000

var errReadMax = errors.New("read bound reached")

// checkNoise runs the noise checks when the bank check passed, else skips
// them like untagged; noise-headless reads the cursors first and the bank
// only when this machine sent a headless session.
func checkNoise(ctx context.Context, env Env, cl *hindsight.Client, tg *target.Target, bank, srv Check) []Check {
	cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home)
	if err != nil {
		cfg = config.Default(env.Dirs, env.Home) // the reference install: backsight's defaults
	}
	var out []Check
	usable := bank.Status == OK || bank.Status == Warn
	if usable {
		history, herr := retain.LoadNoise(env.Dirs.State)
		now := time.Now
		if env.Now != nil {
			now = env.Now
		}
		c := noiseTrend(checkNoiseTransverse(ctx, cl, tg.Bank, cfg.Projects.Transverse), history, now())
		if herr != nil {
			// the trend is blind: say so rather than stay quiet
			c.Status = Warn
			c.Summary += "; its daily readings cannot be read (" + herr.Error() + "), so a growth would go unseen"
			c.Fix = "remove " + retain.NoiseHistoryFile(env.Dirs.State) + ": jobs maintain starts it again"
		}
		out = append(out, c)
		out = append(out, checkNoiseSubagent(ctx, cl, tg.Bank))
	} else {
		why := "skipped: " + skipReason(srv)
		out = append(out, Check{ID: "noise-transverse", Status: Info, Summary: why}, Check{ID: "noise-subagent", Status: Info, Summary: why})
	}
	out = append(out, checkNoiseHeadless(ctx, env, cfg, cl, usable, skipReason(srv)))
	if !usable {
		return append(out, Check{ID: "notes", Status: Info, Summary: "skipped: " + skipReason(srv)})
	}
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	out = append(out, checkNotes(ctx, cl, tg.Bank, cfg.Projects.Transverse[0], now()))
	return append(out, checkLiteralTransverse(ctx, cl, tg, cfg.Projects.Transverse[0]))
}

// checkLiteralTransverse names the documents tagged project:transverse, the
// keyword as written: a replay of memory_store calls that named it used to
// keep it (issue 33), and recall's transverse scope searches the aliases,
// never that tag. retain reattribute leaves notes where they are (their
// project was chosen), so the fix is a re-tag of each document.
func checkLiteralTransverse(ctx context.Context, cl *hindsight.Client, tg *target.Target, alias string) Check {
	c := Check{ID: "transverse-keyword"}
	if alias == retain.TransverseKeyword {
		c.Status, c.Summary = OK, "the canonical transverse alias is the keyword itself"
		return c
	}
	docs, err := cl.TaggedDocumentsSince(ctx, tg.Bank, time.Unix(0, 0), "project:"+retain.TransverseKeyword)
	if err != nil {
		c.Status, c.Summary, c.Detail = Info, "cannot list the documents tagged project:"+retain.TransverseKeyword, err.Error()
		return c
	}
	if len(docs) == 0 {
		c.Status, c.Summary = OK, "no document is tagged project:"+retain.TransverseKeyword
		return c
	}
	c.Status = Warn
	c.Projects = []string{alias}
	c.Summary = fmt.Sprintf("%d document%s tagged project:%s, a pool recall never searches (the transverse pool is project:%s)",
		len(docs), map[bool]string{true: "", false: "s"}[len(docs) == 1], retain.TransverseKeyword, alias)
	var lines []string
	for i, d := range docs {
		if i == headlessFixesMax {
			lines = append(lines, fmt.Sprintf("... and %d more", len(docs)-i))
			break
		}
		tags := make([]string, len(d.Tags))
		for j, t := range d.Tags {
			if t == "project:"+retain.TransverseKeyword {
				t = "project:" + alias
			}
			tags[j] = t
		}
		body, _ := json.Marshal(map[string][]string{"tags": tags})
		lines = append(lines, fmt.Sprintf("curl -X PATCH -H 'Authorization: Bearer $API_KEY' -H 'Content-Type: application/json' -d '%s' '%s%s'",
			body, strings.TrimRight(tg.URL, "/"), "/v1/default/banks/"+strings.ReplaceAll(url.PathEscape(tg.Bank), ":", "%3A")+"/documents/"+strings.ReplaceAll(url.PathEscape(d.ID), ":", "%3A")))
	}
	c.Detail = "documents: " + strings.Join(docIDs(docs), ", ")
	c.Fix = "backsight retain reattribute does not move notes (their project was chosen): re-tag each document by hand (PATCH replaces the tags; API_KEY is in secrets.toml), then the server consolidates it again:\n" + strings.Join(lines, "\n")
	return c
}

func docIDs(docs []hindsight.DocumentItem) []string {
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	return ids
}

// notesWindow: doctor counts the notes stored in the last week.
const notesWindow = 7 * 24 * time.Hour

// checkNotes counts the notes stored in the last week, the transverse
// pool's apart: the memory tools run without a prompt, so what they wrote
// is shown for review (decision 0072 M6). Information, never a fault.
func checkNotes(ctx context.Context, cl *hindsight.Client, bank, alias string, now time.Time) Check {
	c := Check{ID: "notes", Status: OK, Summary: "no note stored in the last 7 days"}
	since := now.Add(-notesWindow)
	n, err := cl.CountTaggedDocumentsSince(ctx, bank, since, "source:note")
	if err != nil {
		c.Status, c.Summary, c.Detail = Info, "cannot count the notes stored this week", err.Error()
		return c
	}
	if n == 0 {
		return c
	}
	pool, err := cl.CountTaggedDocumentsSince(ctx, bank, since, "source:note", "project:"+alias)
	if err != nil {
		c.Status, c.Summary, c.Detail = Info, "cannot count the notes stored this week", err.Error()
		return c
	}
	c.Status = Info
	c.Summary = fmt.Sprintf("%d note%s stored in the last 7 days (%d in the transverse pool)", n, map[bool]string{true: "", false: "s"}[n == 1], pool)
	c.Fix = "review them: backsight memory notes (the transverse pool's: backsight memory notes --project transverse)"
	return c
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
	c.Measure = map[string]float64{"named": float64(named), "total": float64(total)}
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
		c.Detail = strings.TrimPrefix(c.Detail+"; the transverse pool is searched with every project, so another project's notes there reach every prompt", "; ")
		c.Fix = "new sessions are attributed to their repository; clean older ones with backsight forget --project " + alias + " --source subagent --plan, or leave them"
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
// It warns only while the bank still holds a live raw fact of one of their
// documents, so a purge or a forget clears it (M5 review rules).
func checkNoiseHeadless(ctx context.Context, env Env, cfg config.Config, cl *hindsight.Client, usable bool, why string) Check {
	c := Check{ID: "noise-headless", Status: OK, Summary: "no headless session was sent"}
	dir := env.Dirs.CursorsDir()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	res := project.New(env.Home, cfg.Projects.Transverse)
	local := map[string]string{} // session id -> project of its first directory
	for _, f := range files {
		sid := strings.TrimSuffix(filepath.Base(f), ".json")
		cur, ok, err := retain.LoadCursor(dir, sid)
		if err != nil || !ok || cur.Records == 0 || (cur.Bank != "" && cur.Bank != cfg.Server.Bank) {
			continue
		}
		ep, cwd := headlessStart(cur.Transcript)
		if !cfg.Privacy.SkipsEntrypoint(ep) {
			continue
		}
		local[sid] = unknownProject
		if cwd != "" {
			local[sid] = res.Project(cwd)
		}
	}
	if len(local) == 0 {
		return c
	}
	if !usable {
		c.Status, c.Summary = Info, fmt.Sprintf("%d headless session(s) were sent before privacy.skip_entrypoints; the bank was not read: %s", len(local), why)
		return c
	}
	docs, err := cl.ListDocuments(ctx, cfg.Server.Bank)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list the bank's documents", err.Error()
		return c
	}
	type headlessDoc struct{ id, sid, project string }
	var mine []headlessDoc
	others := map[string]bool{} // projects with a document of another session
	for _, d := range docs {
		sid := docSession(d.ID)
		p, headless := local[sid]
		if !headless {
			for _, t := range d.Tags {
				if name, ok := strings.CutPrefix(t, "project:"); ok {
					others[name] = true
				}
			}
			continue
		}
		for _, t := range d.Tags {
			if name, ok := strings.CutPrefix(t, "project:"); ok {
				p = name // the bank's label wins over the first directory's
				break
			}
		}
		mine = append(mine, headlessDoc{d.ID, sid, p})
	}
	more := len(mine) > headlessDocsMax
	if more {
		mine = mine[:headlessDocsMax]
	}
	live := map[string][]string{} // project -> "<fact id> <document>" of its live headless documents
	sessions := map[string]map[string]bool{}
	for _, d := range mine {
		fact, err := liveRawFact(ctx, cl, cfg.Server.Bank, d.id)
		if err != nil {
			c.Status, c.Summary, c.Detail = Warn, "cannot list the memories of document "+d.id, err.Error()
			return c
		}
		if fact == "" {
			continue // purged or forgotten: nothing left to clean
		}
		live[d.project] = append(live[d.project], "backsight forget "+fact+" --purge (document "+d.id+")")
		if sessions[d.project] == nil {
			sessions[d.project] = map[string]bool{}
		}
		sessions[d.project][d.sid] = true
	}
	if len(live) == 0 {
		c.Summary = fmt.Sprintf("the %d headless session(s) sent before privacy.skip_entrypoints hold no memory any more", len(local))
		return c
	}
	perProject := map[string]int{}
	n := 0
	for p, s := range sessions {
		perProject[p] = len(s)
		n += len(s)
	}
	c.Status = Warn
	c.Summary = fmt.Sprintf("%d headless session(s) sent before privacy.skip_entrypoints still have memories: %s", n, topCounts(perProject, len(perProject)))
	c.Detail = "check with --plan that a project holds nothing else worth keeping before forgetting it"
	var fixes, mixed []string
	for _, p := range sortedKeys(perProject) {
		if p != unknownProject {
			c.Projects = append(c.Projects, p)
		}
		// A project that also holds other sessions' documents, the
		// transverse pool or an unknown project: only the headless
		// documents go, one by one.
		if others[p] || p == unknownProject || slices.Contains(cfg.Projects.Transverse, p) {
			mixed = append(mixed, p)
			fixes = append(fixes, live[p]...)
			continue
		}
		fixes = append(fixes, "backsight forget --project "+p+" --plan")
	}
	if len(mixed) > 0 {
		c.Detail += "; " + strings.Join(mixed, ", ") + " also hold other sessions: purge only the headless documents"
	}
	if len(fixes) > headlessFixesMax {
		fixes = append(fixes[:headlessFixesMax], fmt.Sprintf("then run backsight doctor again (%d more)", len(fixes)-headlessFixesMax))
	}
	if more {
		c.Detail += fmt.Sprintf("; only the first %d headless documents were read", headlessDocsMax)
	}
	c.Fix = strings.Join(fixes, "; ")
	return c
}

// docSession is the session id of a session's document (conversation:<sid>,
// conversation:<sid>@<project>, either with a purge's #<n>,
// subagent:<sid>:<agent>), "" for any other.
func docSession(id string) string {
	if rest, ok := strings.CutPrefix(retain.BaseDocumentID(id), "conversation:"); ok {
		sid, _, _ := strings.Cut(rest, "@")
		return sid
	}
	if rest, ok := strings.CutPrefix(id, "subagent:"); ok {
		sid, _, _ := strings.Cut(rest, ":")
		return sid
	}
	return ""
}

// liveRawFact is the id of one live raw fact extracted from document doc,
// "" when none is left; forget <id> --purge deletes that document.
func liveRawFact(ctx context.Context, cl *hindsight.Client, bank, doc string) (string, error) {
	found := ""
	err := cl.ListMemories(ctx, bank, hindsight.MemoryQuery{DocumentID: doc}, func(m hindsight.Memory) error {
		if m.FactType == "observation" || m.State == "invalidated" {
			return nil
		}
		found = m.ID
		return errReadMax
	})
	if errors.Is(err, errReadMax) {
		err = nil
	}
	return found, err
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

// noiseGrowth: a rise of the transverse pool's noise, in points of share
// over a week, that doctor warns about (decision 0072: nothing diverges
// unseen); it must also be noiseGrowthN more observations naming another
// project, and both readings must hold noiseTrendMinN observations, so
// one observation more in a small pool is no alarm.
const (
	noiseGrowth    = 5
	noiseGrowthN   = 3
	noiseTrendMinN = 50
)

// noiseTrend compares c's reading (noise-transverse) with the one jobs
// maintain kept about a week ago: it says the week's change, and warns
// when the share of observations naming another project grew by
// noiseGrowth points or more.
func noiseTrend(c Check, history []retain.NoisePoint, now time.Time) Check {
	if c.Measure == nil || c.Measure["total"] < noiseTrendMinN {
		return c
	}
	prev, ok := retain.WeekBefore(history, now)
	if !ok || prev.Total < noiseTrendMinN {
		return c
	}
	share := 100 * c.Measure["named"] / c.Measure["total"]
	was := 100 * prev.Share()
	c.Summary += fmt.Sprintf(" (%.0f%%, %.0f%% a week ago)", share, was)
	if share-was >= noiseGrowth && int(c.Measure["named"])-prev.Named >= noiseGrowthN {
		c.Status = Warn
		c.Summary += ": growing"
		c.Fix = "backsight retain reattribute --plan shows what today's rules would move; backsight doctor's attribution check shows how often turns are filed elsewhere"
	}
	return c
}

// checkReattribution shows what the last re-attribution (retain
// reattribute, or jobs maintain's daily free moves) left to send again:
// sessions whose documents mix projects, with the cost of fixing them.
func checkReattribution(env Env) Check {
	c := Check{ID: "reattribution", Status: OK}
	st, ok, err := retain.LoadReattributionStatus(env.Dirs.State)
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	switch {
	case err != nil:
		c.Status, c.Summary = Warn, "the last re-attribution's record cannot be read: "+err.Error()
		return c
	case ok && st.Skipped == "components.write is off":
		c.Status, c.Summary = Info, "the daily re-attribution does not run: components.write is off (another integration writes)"
		return c
	case ok && st.Skipped != "":
		// the guard is off for this user: say so plainly
		c.Status = Warn
		c.Summary = "the daily re-attribution does not run (" + st.SkippedAt.UTC().Format("2006-01-02") + "): " + st.Skipped
		c.Fix = "backsight retain reattribute --plan, then backsight retain reattribute to move what it lists"
		return c
	case !ok || st.JudgedAt.IsZero():
		c.Status, c.Summary = Info, "the memory was never judged against today's project rules"
		c.Fix = "backsight retain reattribute --plan (jobs maintain moves what it can every day after its backup)"
		return c
	}
	when := st.JudgedAt.UTC().Format("2006-01-02")
	n := len(st.Pending)
	switch {
	case now().Sub(st.JudgedAt) > reattributionStale:
		c.Status = Warn
		c.Summary = "the memory was last judged against today's project rules on " + when + ": the daily job has not done it since"
		c.Fix = "backsight jobs status; backsight retain reattribute --plan"
	case n == 0:
		c.Summary = "nothing left to send again (judged " + when + ")"
	case st.Declined:
		c.Status = Info
		c.Summary = fmt.Sprintf("%d session(s) mix projects in their documents; sending them again was declined (judged %s)", n, when)
	default:
		c.Status = Warn
		cost := fmt.Sprintf("about %d input tokens", st.Cost.InputTokens)
		if st.Cost.Cost > 0 {
			cost += fmt.Sprintf(", %.2f", st.Cost.Cost)
		}
		c.Summary = fmt.Sprintf("%d session(s) mix projects in their documents (judged %s): sending them again per project costs %s", n, when, cost)
		c.Fix = "backsight retain reattribute --plan, then backsight retain reattribute --answer resend=yes (or resend=no to keep them)"
	}
	return c
}

// reattributionStale: a judgement older than this means the daily job no
// longer judges (stopped, failing).
const reattributionStale = 3 * 24 * time.Hour
