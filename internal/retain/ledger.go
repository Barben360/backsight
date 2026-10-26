package retain

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// TurnEntry is one turn's attribution, kept on this machine only
// (<state>/attribution/<session>/): the project it was filed under, the
// one the path rule gives (decision 0072), and the evidence behind it.
// Its votes are what a later re-attribution and the project families
// read: never the transcripts again, whose worktrees may be gone.
type TurnEntry struct {
	Session string `json:"session"`
	// Turn is its prompt record's uuid, or agent:<id> for a subagent.
	Turn  string    `json:"turn"`
	Agent string    `json:"agent,omitempty"`
	At    time.Time `json:"at"`
	// Base is what the record rule gives (a subagent: its session's
	// directory); Applied is what the turn was filed under; PathRule is
	// what the path rule gives, and Source why.
	Base     string            `json:"base"`
	Applied  string            `json:"applied"`
	PathRule string            `json:"path_rule"`
	Source   string            `json:"source"`
	Votes    attribution.Votes `json:"votes"`
	Mentions []string          `json:"mentions,omitempty"`
}

// Subagent says whether e is a subagent's report.
func (e TurnEntry) Subagent() bool { return e.Agent != "" }

// Attributor applies the path rule (decision 0072) with this machine's
// configuration, and keeps its answers in Dir.
type Attributor struct {
	Resolver   *project.Resolver
	Transverse []string
	Owners     []string // projects.own_owners
	Exclude    []string // privacy.exclude_paths
	Dir        string   // <state>/attribution
	// Deadline, when set, bounds a decision's reading (SubagentStop has
	// 3 s): past it, the report keeps its session's project.
	Deadline time.Time
}

// Sources a subagent's decision can also have.
const (
	SourceIncomplete = "incomplete" // its transcript lagged behind its report
	SourceUnread     = "unread"     // its transcript could not be read
	SourceTimeout    = "timeout"    // no time left to read it
	SourceExcluded   = "excluded"   // its repository was kept out of memory since
)

// ownersFile keeps the remote owners learnt from retained sessions'
// launch repositories (an own owner's repositories are the user's, as in
// E32: the union over every session). It only grows, by that one rule.
func (a Attributor) ownersFile() string { return filepath.Join(a.Dir, "owners.json") }

// owners are the own owners besides a session's launch repository's:
// projects.own_owners and those learnt.
func (a Attributor) owners() []string {
	out := slices.Clone(a.Owners)
	if raw, err := os.ReadFile(a.ownersFile()); err == nil {
		var learnt []string
		if json.Unmarshal(raw, &learnt) == nil {
			out = append(out, learnt...)
		}
	}
	return out
}

// learnOwner adds the owner of a retained launch directory to the owners
// learnt; the caller holds the session's flush lock (one writer at a time
// per session; a lost race only delays the owner to the next session).
func (a Attributor) learnOwner(launchDir string) {
	o := attribution.LaunchOwner(a.Resolver, a.Resolver.Home(), launchDir, a.Exclude)
	if o == "" || a.Dir == "" {
		return
	}
	var learnt []string
	if raw, err := os.ReadFile(a.ownersFile()); err == nil {
		_ = json.Unmarshal(raw, &learnt)
	}
	if slices.Contains(learnt, o) {
		return
	}
	learnt = append(learnt, o)
	slices.Sort(learnt)
	raw, _ := json.Marshal(learnt)
	_ = fsutil.WriteAtomic(a.ownersFile(), raw, 0o600)
}

// repos is the evidence namer of a session launched in launchDir, having
// learnt the worktrees and clones its transcripts made (the session's
// and its subagents'). ok is false when the deadline passed.
func (a Attributor) repos(launchDir, sessionTranscript string) (*attribution.Repos, bool) {
	r := attribution.NewRepos(a.Resolver, a.Resolver.Home(), a.Transverse, launchDir, a.owners(), a.Exclude)
	files := []string{sessionTranscript}
	agents, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(sessionTranscript, ".jsonl"), "subagents", "agent-*.jsonl"))
	for _, p := range append(files, agents...) {
		if a.late() {
			return r, false
		}
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		transcript.Scan(fh, attribution.LearnWords, func(rec transcript.Record) bool {
			r.LearnAll([]transcript.Record{rec}, launchDir)
			return !a.late()
		})
		_ = fh.Close()
	}
	return r, !a.late()
}

func (a Attributor) late() bool { return !a.Deadline.IsZero() && time.Now().After(a.Deadline) }

func (a Attributor) transverse(p string) bool { return slices.Contains(a.Transverse, p) }

// ReportProject is the project a report of e is sent under: its applied
// project, a transverse alias being the canonical one.
func (a Attributor) ReportProject(e TurnEntry) string {
	if a.transverse(e.Applied) {
		return a.Transverse[0]
	}
	return e.Applied
}

// honour applies today's exclusions to a decision: a report moved to a
// repository kept out of memory since (privacy.exclude_paths, pause
// --project, retain = false) goes back to its session's project.
func (a Attributor) honour(e TurnEntry) TurnEntry {
	if e.Applied == e.Base {
		return e
	}
	if root := e.Votes.Roots[e.Applied]; root != "" {
		if out, _ := a.Resolver.OptedOut(root, a.Exclude); out {
			e.Applied, e.Source = e.Base, SourceExcluded
		}
	}
	return e
}

// Subagent is the attribution of subagent agentID's report: the path rule
// over the subagent's own transcript, from base (what its session's
// directory names). The first decision is kept (one per report, whoever
// decides first), so a report rebuilt after a restore lands where the
// first one did, whatever was removed from disk since; only a later
// exclusion of its repository changes it (honour). lastReply is the report
// as the hook got it: a transcript whose last reply differs lags behind,
// and the report then keeps its session's project rather than a decision
// on partial evidence. sessionTranscript is the parent session's. err is
// a decision that could not be kept.
func (a Attributor) Subagent(sid, agentID, sessionTranscript, agentTranscript, base, lastReply string, at time.Time) (TurnEntry, error) {
	if e, ok := a.loadAgent(sid, agentID); ok {
		return a.honour(e), nil
	}
	launch := launchDir(sessionTranscript)
	var repos *attribution.Repos
	e := a.computeSubagent(sid, agentID, launch, func() (*attribution.Repos, bool) {
		if repos == nil {
			r, ok := a.repos(launch, sessionTranscript)
			repos = r
			return r, ok
		}
		return repos, !a.late()
	}, agentTranscript, base, lastReply, at)
	kept, err := a.keepAgent(e)
	return a.honour(kept), err
}

// computeSubagent applies the path rule to a subagent's transcript, with
// the session's evidence namer (made only when the transcript is read).
func (a Attributor) computeSubagent(sid, agentID, launch string, repos func() (*attribution.Repos, bool), agentTranscript, base, lastReply string, at time.Time) TurnEntry {
	e := TurnEntry{Session: sid, Turn: "agent:" + agentID, Agent: agentID, At: at.UTC(), Base: base, Applied: base, PathRule: base, Source: attribution.SourceCWD}
	if a.transverse(base) {
		e.Source = attribution.SourceNone
	}
	fh, err := os.Open(agentTranscript)
	if err != nil {
		e.Source = SourceUnread // kept: a later reader decides the same
		return e
	}
	recs, _, err := transcript.ReadAgent(fh)
	_ = fh.Close()
	if err != nil {
		e.Source = SourceUnread
		return e
	}
	if lastReply != "" && strings.TrimSpace(lastAssistant(recs)) != strings.TrimSpace(lastReply) {
		e.Source = SourceIncomplete
		return e
	}
	r, ok := repos()
	if !ok {
		e.Source = SourceTimeout
		return e
	}
	e.Votes = attribution.Tally(attribution.Calls(recs, launch, a.Resolver.Home()), r)
	if a.late() {
		e.Votes, e.Source = attribution.Votes{}, SourceTimeout
		return e
	}
	prompt := ""
	if len(recs) > 0 && attribution.IsPrompt(recs[0]) {
		prompt = attribution.PromptText(recs[0])
	}
	e.Mentions = attribution.Mentions(prompt, a.names(r.Launch(), nil, e.Votes))
	// A subagent does not inherit its session's previous turn: its own
	// transcript decides, or its session's directory does.
	e.PathRule, e.Source = attribution.Decide(base, a.transverse(base), e.Votes, e.Mentions, attribution.Previous{})
	e.Applied = e.PathRule
	return e
}

// lastAssistant is the text of the last assistant record of recs.
func lastAssistant(recs []transcript.Record) string {
	for i := len(recs) - 1; i >= 0; i-- {
		for _, t := range recs[i].Turns {
			if t.Role == "assistant" {
				return t.Content
			}
		}
	}
	return ""
}

// Redecide applies the current path rule to a recorded subagent turn's
// evidence again, with no transcript read, and today's exclusions: what
// a re-attribution moves a report by. A decision taken without evidence
// (incomplete, unread, timeout) stays as it is.
func (a Attributor) Redecide(e TurnEntry) TurnEntry {
	switch e.Source {
	case SourceIncomplete, SourceUnread, SourceTimeout:
		return e
	}
	e.PathRule, e.Source = attribution.Decide(e.Base, a.transverse(e.Base), e.Votes, e.Mentions, attribution.Previous{})
	e.Applied = e.PathRule
	return a.honour(e)
}

// names are the project names a prompt may mention: the launch project,
// the session's projects so far and this turn's.
func (a Attributor) names(launch string, seen []string, v attribution.Votes) []string {
	var out []string
	if launch != "" && !a.transverse(launch) {
		out = append(out, launch)
	}
	for _, p := range append(slices.Clone(seen), v.Projects()...) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func (a Attributor) agentPath(sid, agentID string) (string, bool) {
	if a.Dir == "" || !session.ValidID(sid) || !session.ValidID(agentID) {
		return "", false
	}
	return filepath.Join(a.Dir, sid, "agent-"+agentID+".json"), true
}

func (a Attributor) loadAgent(sid, agentID string) (TurnEntry, bool) {
	p, ok := a.agentPath(sid, agentID)
	if !ok {
		return TurnEntry{}, false
	}
	raw, err := os.ReadFile(p)
	var e TurnEntry
	if err != nil || json.Unmarshal(raw, &e) != nil || e.Applied == "" {
		return TurnEntry{}, false
	}
	return e, true
}

// keepAgent stores e as the report's decision unless one is stored
// already, which then wins: one decision per report whoever decides first
// (the hook and a rebuild may race). It returns the stored decision.
func (a Attributor) keepAgent(e TurnEntry) (TurnEntry, error) {
	p, ok := a.agentPath(e.Session, e.Agent)
	if !ok {
		return e, nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return e, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".agent-*.tmp")
	if err != nil {
		return e, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return e, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return e, err
	}
	if err := tmp.Close(); err != nil {
		return e, err
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		if stored, found := a.loadAgent(e.Session, e.Agent); found {
			return stored, nil // another writer decided first
		}
		return e, err
	}
	return e, nil
}

// launchDir is the directory a session started in: its first main record
// with one.
func launchDir(sessionTranscript string) string {
	fh, err := os.Open(sessionTranscript)
	if err != nil {
		return ""
	}
	defer func() { _ = fh.Close() }()
	return transcript.FirstCWD(fh)
}

// mainLedger is where the ledger of a session's main turns stands.
type mainLedger struct {
	Transcript  string `json:"transcript"`
	Offset      int64  `json:"offset"` // after the last turn recorded
	Fingerprint string `json:"fingerprint"`
	Launch      string `json:"launch"`
	// the record rule's state at Offset
	Project   string               `json:"project,omitempty"`
	OptOut    string               `json:"opt_out,omitempty"`
	OptOutDir string               `json:"opt_out_dir,omitempty"`
	Previous  attribution.Previous `json:"previous"`
	Names     []string             `json:"names,omitempty"`
}

func (a Attributor) mainPaths(sid string) (state, turns string) {
	d := filepath.Join(a.Dir, sid)
	return filepath.Join(d, "main.json"), filepath.Join(d, "main.jsonl")
}

// recordTurns records the main turns of transcript path that are complete
// (another prompt follows, or the session settled, or force) and not yet
// recorded: on these turns the path rule runs in shadow, its answer kept
// beside the applied one (decision 0072). It returns the new entries. A
// turn kept out of memory (a pause, an excluded directory, a headless
// session) is not recorded. It runs under the session's flush lock.
func (f *Flusher) recordTurns(sid, path string, force bool) []TurnEntry {
	a := f.Attribution
	if a.Dir == "" || a.Resolver == nil || !session.ValidID(sid) {
		return nil
	}
	statePath, turnsPath := a.mainPaths(sid)
	var st mainLedger
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if st.Transcript != path || fi.Size() < st.Offset || (st.Offset > 0 && !sameFingerprint(path, st.Offset, st.Fingerprint)) {
		// a new or rewritten transcript: recorded again from its start
		st = mainLedger{Transcript: path}
		_ = os.Remove(turnsPath)
	}
	if fi.Size() == st.Offset {
		return nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	if _, err := fh.Seek(st.Offset, 0); err != nil {
		_ = fh.Close()
		return nil
	}
	recs, _, err := transcript.Read(fh, st.Offset)
	_ = fh.Close()
	if err != nil || len(recs) == 0 {
		return nil
	}
	if st.Offset == 0 {
		for _, r := range recs {
			if r.CWD != "" {
				st.Launch = r.CWD
				break
			}
		}
		a.learnOwner(st.Launch)
	}
	now := f.Now()
	attrs := f.attribute(sid, recs, attr{project: st.Project, namedOut: st.OptOut, namedOutDir: st.OptOutDir})
	settled := force || now.Sub(clampTime(recs[len(recs)-1].Timestamp, fi.ModTime(), now)) >= SettleAfter
	var starts []int
	for i, r := range recs {
		if attribution.IsPrompt(r) {
			starts = append(starts, i)
		}
	}
	var repos *attribution.Repos
	var out []TurnEntry
	advance := func(last transcript.Record) {
		at := attrs[last.End]
		st.Offset, st.Project, st.OptOut, st.OptOutDir = last.End, at.project, at.namedOut, at.namedOutDir
	}
	if len(starts) > 0 && starts[0] > 0 {
		advance(recs[starts[0]-1]) // the tail of a turn recorded before
	} else if len(starts) == 0 && settled {
		advance(recs[len(recs)-1])
	}
	for k, i := range starts {
		j := len(recs)
		if k+1 < len(starts) {
			j = starts[k+1]
		}
		if j == len(recs) && !settled {
			break // still running
		}
		turn := recs[i:j]
		advance(turn[len(turn)-1])
		if f.keptOut(turn, attrs) {
			st.Previous = attribution.Previous{} // never carried past a kept-out turn
			continue
		}
		if repos == nil {
			repos, _ = a.repos(st.Launch, path) // no deadline in a flush
		}
		e := TurnEntry{Session: sid, Turn: turn[0].UUID, At: turn[0].Timestamp.UTC(), Base: baseOf(turn, attrs)}
		e.Applied = e.Base
		e.Votes = attribution.Tally(attribution.Calls(turn, st.Launch, a.Resolver.Home()), repos)
		e.Mentions = attribution.Mentions(attribution.PromptText(turn[0]), a.names(repos.Launch(), st.Names, e.Votes))
		e.PathRule, e.Source = attribution.Decide(e.Base, a.transverse(e.Base), e.Votes, e.Mentions, st.Previous)
		st.Previous = attribution.Previous{Project: e.PathRule, Source: e.Source}
		for _, p := range e.Votes.Projects() {
			if !slices.Contains(st.Names, p) {
				st.Names = append(st.Names, p)
			}
		}
		out = append(out, e)
	}
	if len(out) > 0 {
		var buf bytes.Buffer
		for _, e := range out {
			raw, _ := json.Marshal(e)
			buf.Write(append(raw, '\n'))
		}
		if err := appendFile(turnsPath, buf.Bytes()); err != nil {
			return nil // the state is not moved: recorded next time
		}
	}
	if fp, err := Fingerprint(path, st.Offset); err == nil {
		st.Fingerprint = fp
	}
	raw, _ := json.Marshal(st)
	_ = fsutil.WriteAtomic(statePath, raw, 0o600)
	return out
}

func sameFingerprint(path string, offset int64, fp string) bool {
	got, err := Fingerprint(path, offset)
	return err == nil && got == fp
}

func appendFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// keptOut: a record of the turn is kept out of memory.
func (f *Flusher) keptOut(turn []transcript.Record, attrs map[int64]attr) bool {
	for _, r := range turn {
		if attrs[r.End].out || f.headless(r.Entrypoint) {
			return true
		}
	}
	return false
}

// baseOf is the project most of the turn's records are filed under (the
// first one on a tie).
func baseOf(turn []transcript.Record, attrs map[int64]attr) string {
	var order []string
	n := map[string]int{}
	for _, r := range turn {
		p := attrs[r.End].project
		if n[p] == 0 {
			order = append(order, p)
		}
		n[p]++
	}
	best := order[0]
	for _, p := range order[1:] {
		if n[p] > n[best] {
			best = p
		}
	}
	return best
}

// Turns reads every recorded turn of this machine whose time is at or
// after since: main turns and subagent reports, each once (the last
// record of a turn wins).
func (a Attributor) Turns(since time.Time) ([]TurnEntry, error) {
	dirs, err := os.ReadDir(a.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TurnEntry
	for _, d := range dirs {
		if !d.IsDir() || !session.ValidID(d.Name()) {
			continue
		}
		seen := map[string]int{}
		keep := func(e TurnEntry) {
			if e.At.Before(since) || e.Turn == "" {
				return
			}
			if i, ok := seen[e.Turn]; ok {
				out[i] = e
				return
			}
			seen[e.Turn] = len(out)
			out = append(out, e)
		}
		_, turnsPath := a.mainPaths(d.Name())
		if fh, err := os.Open(turnsPath); err == nil {
			sc := bufio.NewScanner(fh)
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				var e TurnEntry
				if json.Unmarshal(sc.Bytes(), &e) == nil {
					keep(e)
				}
			}
			_ = fh.Close()
		}
		agents, _ := filepath.Glob(filepath.Join(a.Dir, d.Name(), "agent-*.json"))
		for _, p := range agents {
			var e TurnEntry
			if raw, err := os.ReadFile(p); err == nil && json.Unmarshal(raw, &e) == nil {
				keep(e)
			}
		}
	}
	return out, nil
}

// AttributionKeep is how long a session's recorded turns and decisions
// are kept after its last change: longer than the families' window, and
// a report's decision also travels in its document's metadata.
const AttributionKeep = 120 * 24 * time.Hour

// PruneAttribution removes the records of sessions untouched since
// before now - keep.
func (a Attributor) PruneAttribution(now time.Time, keep time.Duration) {
	dirs, err := os.ReadDir(a.Dir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || !session.ValidID(d.Name()) {
			continue
		}
		p := filepath.Join(a.Dir, d.Name())
		newest := time.Time{}
		files, _ := os.ReadDir(p)
		for _, f := range files {
			if fi, err := f.Info(); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if !newest.IsZero() && now.Sub(newest) > keep {
			_ = os.RemoveAll(p)
		}
	}
}
