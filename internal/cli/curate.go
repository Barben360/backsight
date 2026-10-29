package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

const (
	curateTimeout   = 30 * time.Second
	projectsTimeout = 20 * time.Second
)

// maintenanceRefusal: curation waits while an update, a backup or a restore
// holds the maintenance lock (SPEC §11.4); a purge during a restore could
// be undone silently.
func maintenanceRefusal() error {
	d, _, err := machine()
	if err != nil {
		return err
	}
	if held := state.Inspect(d.MaintenanceLock()); held != nil {
		return heldRefusal(held)
	}
	return nil
}

func heldRefusal(held *state.HeldError) error {
	switch {
	case held.Stale:
		return &output.Error{Code: output.Blocked, Message: "a maintenance lock is left by " + held.Owner.Command + ", whose process is gone: backsight doctor tells how to clear it"}
	default:
		return &output.Error{Code: output.Blocked, Message: "maintenance in progress (" + held.Owner.Command + " since " + held.Owner.Since.Format(time.RFC3339) + "): try again when it is over"}
	}
}

func curateClient() (*hindsight.Client, string, error) {
	if err := maintenanceRefusal(); err != nil {
		return nil, "", err
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return nil, "", err
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, curateTimeout)
	if err != nil {
		return nil, "", blocked(err)
	}
	return cl, cfg.Server.Bank, nil
}

// curateWriter is curateClient for a command that writes to the server.
// gate refuses, before a write, once connect moved this PC to another
// server since the config was read (the sends generation, read first).
func curateWriter() (cl *hindsight.Client, bank string, gate func() error, err error) {
	d, _, err := machine()
	if err != nil {
		return nil, "", nil, err
	}
	generation := sendsGeneration(d) // before the config it goes with
	if cl, bank, err = curateClient(); err != nil {
		return nil, "", nil, err
	}
	sendsConfigLoaded()
	sent := sendsGate(d, generation)
	return cl, bank, func() error {
		if err := sent(); err != nil {
			return &output.Error{Code: output.Blocked, Message: startedOverText(err, "this command") + ": nothing was changed; run it again"}
		}
		return nil
	}, nil
}

// heldWrites refuses a write to the server while another server answers
// at the joined address (decision 0073), before any write.
func heldWrites() error {
	d, _, err := machine()
	if err != nil {
		return err
	}
	cfg, _, err := loadServerConfig()
	if err != nil {
		return err
	}
	return otherServerRefusal(d, cfg)
}

type forgetResult struct {
	ID          string     `json:"id"`
	Action      string     `json:"action"` // forgotten | restored | purge_planned | purged | partly_purged
	Sources     []string   `json:"sources,omitempty"`
	Documents   []purgeDoc `json:"documents,omitempty"`
	Undeletable []string   `json:"undeletable,omitempty"` // facts with no document: retired, kept in Hindsight's archive
	Pages       []string   `json:"pages,omitempty"`       // briefing pages deleted (they may quote the text; rewritten at the next session)
	Backups     []string   `json:"backups,omitempty"`     // archives taken before the purge: they still hold it
	Note        string     `json:"note,omitempty"`
}

// purgeDoc is a document a purge deletes, with every memory extracted from
// it: one secret costs the whole conversation segment.
type purgeDoc struct {
	ID       string `json:"id"`
	Memories int    `json:"memories"`
	Selected int    `json:"selected,omitempty"` // forget --project: the selected raw facts in it
	// tomb: the document's tombstone, read from its metadata.
	tomb state.Tombstone
}

// newForgetCmd is `forget <id> [--purge [--yes]] [--restore]` and `forget
// --project P [--source S] [--purge] [--plan]` (SPEC §2, §13, decision 0071
// C7): the only way memory is deleted, always by the user.
func newForgetCmd(e *env) *cobra.Command {
	var purge, yes, restore bool
	var pf forgetProjectFlags
	cmd := &cobra.Command{
		Use:   "forget <id> | --project <name> [--source <s>]",
		Short: "Retire a memory or a whole project (reversible), or with --purge delete it with its source facts and documents",
		Long: "forget <id> retires one memory; --purge deletes it with its source facts and documents (confirmed with --yes). " +
			"forget --project P retires every memory of project P (with --source S, only its raw facts tagged source:S and the observations drawn from them); " +
			"--purge deletes their documents and observations. --plan lists the documents and counts; the write needs --answer confirm_project=P.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if purge && restore {
				return &output.Error{Code: output.Usage, Message: "--purge and --restore exclude each other"}
			}
			if pf.project != "" || cmd.Flags().Changed("project") {
				switch {
				case len(args) > 0:
					return &output.Error{Code: output.Usage, Message: "forget <id> and --project exclude each other"}
				case restore:
					return &output.Error{Code: output.Usage, Message: "--restore undoes one forget: backsight forget --restore <id>"}
				case yes:
					return &output.Error{Code: output.Usage, Message: "--project is confirmed with --answer confirm_project=<name>, not --yes"}
				}
				pf.purge = purge
				res, err := runForgetProject(cmd.Context(), pf)
				return e.report("forget", res, func() string { return forgetProjectText(res) }, err)
			}
			switch {
			case pf.source != "" || pf.plan || len(pf.answers) > 0:
				return &output.Error{Code: output.Usage, Message: "--source, --plan and --answer go with --project"}
			case len(args) != 1:
				return &output.Error{Code: output.Usage, Message: "forget needs a memory id, or --project <name>"}
			}
			cl, bank, gate, err := curateWriter()
			if err != nil {
				return err
			}
			ctx, id := cmd.Context(), args[0]
			res := forgetResult{ID: id}
			switch {
			case restore, !purge:
				if err := heldWrites(); err != nil {
					return err
				}
				// A memory hidden on this machine, or on another one whose
				// hiding recall here follows, was never invalidated on the
				// server: restoring it there may find nothing, and its mark
				// is cleared here all the same.
				var local, remote bool
				if restore {
					local, remote = hiddenHere(id)
				}
				hidden := local || remote
				// Only raw facts can be curated: an observation is retired
				// through its source facts and regenerates without them.
				targets, err := curatable(ctx, cl, bank, id)
				if err != nil && !hidden {
					return curateError(err, id)
				}
				reason := map[bool]string{true: "restored by the user (backsight forget --restore)", false: "forgotten by the user (backsight forget)"}[restore]
				if err := gate(); err != nil {
					return err
				}
				var done []string
				for _, t := range targets {
					if err = cl.Invalidate(ctx, bank, t, reason, restore); err != nil {
						if !hidden {
							return curateError(err, t)
						}
						break
					}
					done = append(done, t)
				}
				res.Action = map[bool]string{true: "restored", false: "forgotten"}[restore]
				if len(done) > 0 && (len(done) != 1 || done[0] != id) {
					res.Sources = done
				}
				if hidden {
					res.Note = "no longer marked hidden on this machine"
					if !local {
						res.Note = "hidden by another machine, visible again on this machine"
					}
					if err != nil {
						res.Note += " (on the server: " + err.Error() + ")"
					}
				}
				if restore {
					journalUnhide(append([]string{id}, res.Sources...))
				} else {
					journalForget(append([]string{id}, res.Sources...))
				}
			default:
				res, err = purgeMemory(ctx, cl, bank, id, yes, gate)
				if err != nil {
					return curateError(err, id)
				}
				if !yes {
					return e.report("forget", res, func() string { return forgetText(res) }, &output.Error{Code: output.NeedsUser,
						Message: "purging deletes the documents listed (every fact extracted from them): confirm with backsight forget " + id + " --purge --yes",
						Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight forget " + id + " --purge --yes", Why: "delete for good what is listed"}}})
				}
				journalForget(append([]string{id}, res.Sources...))
			}
			return e.emit("forget", res, func() string { return forgetText(res) })
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "delete the memory, its source facts and the documents they came from (leaked secret)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --purge")
	cmd.Flags().BoolVar(&restore, "restore", false, "undo a plain forget, or a retirement Claude reported")
	cmd.Flags().StringVar(&pf.project, "project", "", "every memory of this project, on the configured bank")
	cmd.Flags().StringVar(&pf.source, "source", "", "with --project: only the raw facts tagged source:<s> (chat, subagent, note…) and the observations drawn from them")
	cmd.Flags().BoolVar(&pf.plan, "plan", false, "with --project: list the documents and counts, changing nothing")
	cmd.Flags().StringArrayVar(&pf.answers, "answer", nil, "with --project: confirm_project=<name>")
	return cmd
}

// curatable are the raw facts behind id: id itself for a world or
// experience fact, its source facts for an observation (the API curates
// raw facts only).
func curatable(ctx context.Context, cl *hindsight.Client, bank, id string) ([]string, error) {
	m, err := cl.GetMemory(ctx, bank, id)
	if err != nil {
		return nil, err
	}
	if m.Type != "observation" {
		return []string{id}, nil
	}
	var out []string
	for _, s := range m.Sources {
		out = append(out, s.ID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("observation %s lists no source facts to retire", id)
	}
	return out, nil
}

// purgeMemory plans, and with yes performs, the purge (Hindsight 1.1.0
// has no single-memory delete). The raw facts behind id are read one by
// one, since an observation's source list carries no document; purgeWrite
// does the writes. A fact with no document stays in the archive: the purge
// says so instead of claiming it.
func purgeMemory(ctx context.Context, cl *hindsight.Client, bank, id string, yes bool, gate func() error) (forgetResult, error) {
	res := forgetResult{ID: id, Action: "purge_planned"}
	m, err := cl.GetMemory(ctx, bank, id)
	if err != nil {
		return res, err
	}
	facts := []hindsight.MemoryDetail{m}
	if m.Type == "observation" {
		facts = nil
		for _, s := range m.Sources {
			f, err := cl.GetMemory(ctx, bank, s.ID)
			if err != nil {
				return res, fmt.Errorf("reading source fact %s: %w", s.ID, err)
			}
			facts = append(facts, f)
			res.Sources = append(res.Sources, s.ID)
		}
		if len(facts) == 0 {
			return res, fmt.Errorf("observation %s lists no source facts to purge", id)
		}
	}
	if res.Documents, res.Undeletable, err = purgeDocuments(ctx, cl, bank, facts); err != nil || !yes {
		return res, err
	}
	var mirror string
	res.Pages, mirror, err = purgeWrite(ctx, cl, bank, gate, facts, res.Documents, projectsOf(append([]hindsight.MemoryDetail{m}, facts...)),
		"rerun on one of the source facts: backsight forget <fact id> --purge")
	if err != nil {
		return res, err
	}
	res.Action = "purged"
	res.Note = "backups taken before now still hold it: delete them, or keep them out of reach, if the text was a secret"
	if mirror != "" {
		res.Note += "; " + mirror
	}
	res.Backups = backupsBefore(now())
	if len(res.Undeletable) > 0 {
		res.Action = "partly_purged"
		res.Note = "the facts with no document are retired but stay in Hindsight's archive (no API deletes them); " + res.Note
	}
	return res, nil
}

// purgeDocuments are the documents a purge of facts deletes, each read for
// its memory count, and the facts with no document, which no API deletes.
func purgeDocuments(ctx context.Context, cl *hindsight.Client, bank string, facts []hindsight.MemoryDetail) ([]purgeDoc, []string, error) {
	var out []purgeDoc
	var undeletable []string
	docs := map[string]bool{}
	for _, f := range facts {
		if f.DocumentID == "" {
			undeletable = append(undeletable, f.ID)
		} else {
			docs[f.DocumentID] = true
		}
	}
	for d := range docs {
		doc, err := cl.GetDocument(ctx, bank, d)
		if err != nil {
			return out, undeletable, fmt.Errorf("reading document %s: %w", d, err)
		}
		tomb := retain.TombstoneOf(bank, d, doc.Metadata, time.Time{})
		if strings.HasPrefix(d, "note:") && doc.OriginalText != "" {
			tomb.ContentSHA256 = retain.ContentHash(doc.OriginalText) // a backfill replays it under another id
		}
		out = append(out, purgeDoc{ID: d, Memories: doc.MemoryUnits, tomb: tomb})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, undeletable, nil
}

// projectsOf are the projects ms are tagged with.
func projectsOf(ms []hindsight.MemoryDetail) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		for _, t := range m.Tags {
			if p, ok := strings.CutPrefix(t, "project:"); ok {
				out[p] = true
			}
		}
	}
	return out
}

// purgeWrite performs a planned purge and returns the briefing pages it
// deleted, and a note when the control bank could not take the tombstones.
// First each document's tombstone goes into state.json, so that nothing
// sends its records again (M5 review rules); then each live fact has its
// observations cleared while the API still sees it, then is retired; then
// the documents go, and with them every fact, live or archived, extracted
// from them; last, the tombstones are mirrored into the control bank, for
// the other clients and a reinstall. rerun says how to finish when a
// document delete fails. gate (curateWriter's) is checked under the lock,
// which connect takes too.
func purgeWrite(ctx context.Context, cl *hindsight.Client, bank string, gate func() error, facts []hindsight.MemoryDetail, docs []purgeDoc, projects map[string]bool, rerun string) ([]string, string, error) {
	// The writes hold the maintenance lock: a backup cannot dump a half-purged
	// database, and one that dumped before is listed below, done or not.
	d, _, err := machine()
	if err != nil {
		return nil, "", err
	}
	// Neither its deletes nor its tombstones go to another server.
	cfg, _, err := loadServerConfig()
	if err != nil {
		return nil, "", err
	}
	if err := otherServerRefusal(d, cfg); err != nil {
		return nil, "", err
	}
	l, err := state.TryAcquire(d.MaintenanceLock(), "forget --purge")
	var held *state.HeldError
	if errors.As(err, &held) {
		return nil, "", heldRefusal(held)
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = l.Release() }()
	if err := gate(); err != nil {
		return nil, "", err
	}
	// Before any delete: a purge stopped half way still holds what it
	// deleted. A tombstone of a document left in place costs nothing (the
	// server holds those records anyway).
	at := now().UTC().Truncate(time.Second)
	tombs := make([]state.Tombstone, 0, len(docs))
	for _, doc := range docs {
		t := doc.tomb
		if t.Document == "" { // not read through purgeDocuments
			t = retain.TombstoneOf(bank, doc.ID, nil, at)
		}
		t.PurgedAt = at
		tombs = append(tombs, t)
	}
	if err := state.UpdateWithin(d, "forget --purge", 5*time.Second, func(s *state.State) error {
		s.AddTombstones(tombs...)
		return nil
	}); err != nil {
		return nil, "", fmt.Errorf("recording what the purge deletes (nothing was deleted): %w", err)
	}
	pages, err := purgeDelete(ctx, cl, bank, facts, docs, projects, rerun)
	if err != nil {
		return pages, "", err
	}
	return pages, mirrorTombstones(ctx, cl, d, bank), nil
}

// mirrorTombstones writes this client's tombstones of bank into its control
// bank; a failure is a note, not an error: the purge is done, this client
// holds them, and its next jobs tick writes them (retain.Reconcile).
func mirrorTombstones(ctx context.Context, cl *hindsight.Client, d paths.Dirs, bank string) string {
	st, err := state.Load(d.StateFile())
	if err == nil {
		_, err = retain.WriteTombstones(ctx, cl, bank, st.TombstonesOf(bank))
	}
	if err != nil {
		return "the purge is recorded on this machine, but not yet in bank " + retain.ControlBank(bank) + " (" + err.Error() + "): the next jobs tick writes it there, for the other machines and a reinstall"
	}
	return ""
}

// purgeDelete is purgeWrite's writes on the memory bank (retain.PurgeWrites,
// which a restore and a reconcile share to purge a document again).
func purgeDelete(ctx context.Context, cl *hindsight.Client, bank string, facts []hindsight.MemoryDetail, docs []purgeDoc, projects map[string]bool, rerun string) ([]string, error) {
	pf := make([]retain.PurgeFact, 0, len(facts))
	for _, f := range facts {
		pf = append(pf, retain.PurgeFact{ID: f.ID, State: f.State})
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	_, pages, err := retain.PurgeWrites(ctx, cl, bank, pf, ids, projects, rerun)
	return pages, err
}

func curateError(err error, id string) error {
	var apiErr *hindsight.APIError
	// Only a read of the asked id itself means "no such memory"; any other
	// NotFound (a source fact, a document) is the server's state, reported.
	if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound && apiErr.Method == http.MethodGet &&
		strings.HasSuffix(apiErr.Path, "/memories/"+url.PathEscape(id)) {
		return &output.Error{Code: output.Usage, Message: "no memory " + id}
	}
	return blocked(err)
}

func forgetText(r forgetResult) string {
	switch r.Action {
	case "forgotten", "restored":
		what := r.ID
		if len(r.Sources) > 0 {
			what = fmt.Sprintf("%s (through its %d source fact(s): observations are derived)", r.ID, len(r.Sources))
		}
		if r.Action == "restored" && r.Note != "" {
			return what + " restored: " + r.Note + "\n"
		}
		if r.Action == "restored" {
			return what + " restored\n"
		}
		return fmt.Sprintf("%s forgotten: out of recall and consolidation (undo: backsight forget --restore %s)\n", what, r.ID)
	}
	var b strings.Builder
	verb := map[bool]string{true: "deleted", false: "would delete"}[r.Action != "purge_planned"]
	fmt.Fprintf(&b, "%s: memory %s, %d source fact(s), document(s):\n", verb, r.ID, len(r.Sources))
	for _, d := range r.Documents {
		fmt.Fprintf(&b, "- %s (with all %d memories extracted from it)\n", d.ID, d.Memories)
	}
	if len(r.Undeletable) > 0 {
		fmt.Fprintf(&b, "not deletable (no document, retired only): %s\n", strings.Join(r.Undeletable, ", "))
	}
	for _, p := range r.Pages {
		fmt.Fprintf(&b, "- briefing page %s (rewritten at the next session)\n", p)
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	for _, p := range r.Backups {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

type scanFinding struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	State   string   `json:"state"` // live | forgotten
	Project string   `json:"project"`
	Kinds   []string `json:"kinds"`
	Preview string   `json:"preview"` // redacted
}

// newMemoryCmd groups `memory scan` (SPEC §13 threats), `memory notes`
// and `memory stale`.
func newMemoryCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "memory", Short: "Inspect the memory itself"}
	cmd.AddCommand(newMemoryNotesCmd(e))
	cmd.AddCommand(newMemoryStaleCmd(e))
	cmd.AddCommand(&cobra.Command{
		Use:   "scan",
		Short: "List stored memories that look like they hold a secret, with the command that purges each",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			findings := []scanFinding{}
			// A plain forget keeps the text on the server (archive, exports):
			// soft-forgotten facts are scanned too. Observations are never
			// archived.
			passes := []struct{ ft, state string }{{"world", ""}, {"experience", ""}, {"observation", ""}, {"world", "invalidated"}, {"experience", "invalidated"}}
			for _, p := range passes {
				err := cl.EachMemory(cmd.Context(), bank, p.ft, p.state, func(m hindsight.Memory) error {
					red, kinds := redact.Text(m.Text)
					if len(kinds) > 0 {
						findings = append(findings, scanFinding{ID: m.ID, Type: p.ft, State: map[bool]string{true: "forgotten", false: "live"}[p.state != ""],
							Project: recall.ProjectOf(m.Tags), Kinds: kinds, Preview: clip(red, 100)})
					}
					return nil
				})
				if err != nil {
					return blocked(err)
				}
			}
			// Briefing pages are server-written from the memories: a secret
			// can be quoted there (forget --purge on a source deletes them).
			models, err := cl.MentalModels(cmd.Context(), bank)
			if err != nil {
				return blocked(err)
			}
			for _, m := range models {
				if !strings.HasPrefix(m.ID, briefingModelPrefix) {
					continue
				}
				if red, kinds := redact.Text(m.Content); len(kinds) > 0 {
					findings = append(findings, scanFinding{ID: m.ID, Type: "briefing page", State: "live", Project: recall.ProjectOf(m.Tags), Kinds: kinds, Preview: clip(red, 100)})
				}
			}
			return e.emit("memory scan", findings, func() string {
				if len(findings) == 0 {
					return "no secret shape found in the memory\n"
				}
				var b strings.Builder
				for _, f := range findings {
					fix := "backsight forget " + f.ID + " --purge"
					if f.Type == "briefing page" {
						fix = "purge the memory it quotes (listed above or below); that deletes this page too"
					}
					fmt.Fprintf(&b, "%s (%s, %s, %s, %s): %s\n  %s\n", f.ID, f.Type, f.State, f.Project, strings.Join(f.Kinds, ", "), f.Preview, fix)
				}
				return b.String()
			})
		},
	})
	return cmd
}

type askResult struct {
	Answer       string `json:"answer"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// newAskCmd is `ask <question>` (Hindsight reflect): an agentic LLM loop
// over the memory, scoped to the project and the transverse aliases unless --all.
func newAskCmd(e *env) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the server's LLM a question over the memory (an LLM loop over many memories: costly, tokens shown)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := maintenanceRefusal(); err != nil {
				return err
			}
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			_, home, err := machine()
			if err != nil {
				return err
			}
			cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 3*time.Minute)
			if err != nil {
				return blocked(err)
			}
			q, _ := redact.Text(strings.Join(args, " "))
			var tags []string
			if !all {
				tags = []string{"project:" + currentProject(home, cfg)}
				for _, t := range cfg.Projects.Transverse {
					tags = append(tags, "project:"+t)
				}
			}
			a, err := cl.Reflect(cmd.Context(), cfg.Server.Bank, q, tags, 1000)
			if err != nil {
				return blocked(err)
			}
			res := askResult{Answer: a.Text, InputTokens: a.Usage.InputTokens, OutputTokens: a.Usage.OutputTokens}
			return e.emit("ask", res, func() string {
				return fmt.Sprintf("%s\n\n(%d input, %d output tokens)\n", strings.TrimSpace(res.Answer), res.InputTokens, res.OutputTokens)
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "the whole memory, not only this project and the transverse one")
	return cmd
}

type projectInfo struct {
	Name     string    `json:"name"`
	Memories int       `json:"memories"`
	Last     time.Time `json:"last"`
	// Family: the projects worked on together with it on this machine
	// (searched with it when recall.families is on).
	Family []string `json:"family,omitempty"`
}

// newProjectsCmd lists the projects of the bank with counts and last
// activity.
func newProjectsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "List the projects in the memory with their memory counts and last activity",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, bank, err := curateClient()
			if err != nil {
				return err
			}
			tags, err := cl.Tags(cmd.Context(), bank, "project:")
			if err != nil {
				return blocked(err)
			}
			// Last activity is one read per project: four at a time, all
			// within one bound; a project whose read fails shows "-".
			list := make([]projectInfo, len(tags))
			ctx, cancel := context.WithTimeout(cmd.Context(), projectsTimeout)
			defer cancel()
			var wg sync.WaitGroup
			sem := make(chan struct{}, 4)
			for i, t := range tags {
				list[i] = projectInfo{Name: strings.TrimPrefix(t.Tag, "project:"), Memories: t.Count}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if m, err := cl.Recent(ctx, bank, t.Tag, "", 1); err == nil && len(m) > 0 {
						list[i].Last = m[0].When()
					}
				}()
			}
			wg.Wait()
			sort.SliceStable(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
			if d, _, err := machine(); err == nil {
				if fam, err := retain.LoadFamilies(d.FamiliesFile()); err == nil {
					for i := range list {
						list[i].Family = fam.Of[list[i].Name]
					}
				}
			}
			return e.emit("projects", list, func() string {
				var b strings.Builder
				for _, p := range list {
					last := "-"
					if !p.Last.IsZero() {
						last = p.Last.Format("2006-01-02")
					}
					fmt.Fprintf(&b, "%-30s %6d  %s\n", safeProject(p.Name), p.Memories, last)
					if len(p.Family) > 0 {
						fam := make([]string, len(p.Family))
						for i, f := range p.Family {
							fam[i] = safeProject(f)
						}
						fmt.Fprintf(&b, "  worked on with: %s\n", strings.Join(fam, ", "))
					}
				}
				return b.String()
			})
		},
	}
}

// backupsBefore lists the backups taken before t (decision 0056 block 1:
// the purge says where the text still is).
func backupsBefore(t time.Time) []string {
	cfg, _, err := loadServerConfig()
	if err != nil {
		return nil
	}
	d, _, err := machine()
	if err != nil {
		return nil
	}
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(d.Data, "backups")
	}
	var out []string
	for _, e := range backup.Before(dir, t) {
		out = append(out, e.Path)
	}
	return out
}
