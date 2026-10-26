package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
)

type forgetProjectFlags struct {
	project, source string
	purge, plan     bool
	answers         []string
}

// resume is the command that runs this selection again, the answers kept.
func (f forgetProjectFlags) resume() string {
	cmd := "backsight forget --project " + shellWord(f.project)
	if f.source != "" {
		cmd += " --source " + shellWord(f.source)
	}
	if f.purge {
		cmd += " --purge"
	}
	for _, a := range f.answers {
		cmd += " --answer " + shellWord(a)
	}
	return cmd
}

// memoryCounts are the memories of a selection by type. Forgotten are its
// raw facts already retired: in Hindsight's archive, reached by --purge only.
type memoryCounts struct {
	World       int `json:"world"`
	Experience  int `json:"experience"`
	Observation int `json:"observation"`
	Forgotten   int `json:"forgotten"`
}

type forgetProjectResult struct {
	Project string `json:"project"`
	Source  string `json:"source,omitempty"`
	Bank    string `json:"bank"`
	// Action: forget_planned | forgotten | purge_planned | purged | partly_purged
	Action   string       `json:"action"`
	Memories memoryCounts `json:"memories"`
	// Documents: those of the selected raw facts; a purge deletes them with
	// every memory extracted from them, selected or not.
	Documents        []purgeDoc `json:"documents"`
	Retired          int        `json:"retired,omitempty"`           // raw facts this run retired
	Undeletable      []string   `json:"undeletable,omitempty"`       // facts with no document: retired, kept in the archive
	ObservationsLeft []string   `json:"observations_left,omitempty"` // selected observations still there after the purge
	Pages            []string   `json:"pages,omitempty"`
	Backups          []string   `json:"backups,omitempty"`
	Note             string     `json:"note,omitempty"`
}

// projectSelection is what forget --project acts on.
type projectSelection struct {
	facts    []hindsight.MemoryDetail // live raw facts, and with --purge the archived ones
	obs      map[string]bool          // live observations, by id
	obsTags  [][]string
	archived int
}

// runForgetProject is forget --project (decision 0071 C7), on the
// configured bank only. The whole selection is listed before any write:
// retiring facts while paging would shift the pages.
func runForgetProject(ctx context.Context, f forgetProjectFlags) (forgetProjectResult, error) {
	res := forgetProjectResult{Project: f.project, Source: f.source, Action: map[bool]string{true: "purge_planned", false: "forget_planned"}[f.purge], Documents: []purgeDoc{}}
	if strings.TrimSpace(f.project) == "" {
		return res, &output.Error{Code: output.Usage, Message: "--project needs a project name"}
	}
	answers, err := parseAnswers(f.answers, "")
	if err != nil {
		return res, err
	}
	for k := range answers {
		if k != "confirm_project" {
			return res, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": forget --project asks confirm_project"}
		}
	}
	got, confirmed := answers["confirm_project"]
	if confirmed && got != f.project {
		return res, &output.Error{Code: output.Usage, Message: fmt.Sprintf("the project name %q is not the one to forget (%s): nothing was changed", got, f.project)}
	}
	cl, bank, err := curateClient()
	if err != nil {
		return res, err
	}
	res.Bank = bank
	sel, err := selectProject(ctx, cl, bank, f.project, f.source, f.purge)
	if err != nil {
		return res, blocked(err)
	}
	selected := map[string]int{}
	for _, m := range sel.facts {
		switch {
		case m.State == "invalidated":
			res.Memories.Forgotten++
		case m.Type == "experience":
			res.Memories.Experience++
		default:
			res.Memories.World++
		}
		selected[m.DocumentID]++
	}
	res.Memories.Observation = len(sel.obs)
	if !f.purge {
		res.Memories.Forgotten = sel.archived
	}
	// A plain forget retires raw facts: with none live, it has nothing to do.
	if len(sel.facts) == 0 && (!f.purge || len(sel.obs) == 0) {
		what := "no live raw fact of project " + f.project
		if f.purge {
			what = "no memory of project " + f.project
		}
		if f.source != "" {
			what += " from source " + f.source
		}
		what += " in bank " + bank
		if !f.purge && len(sel.obs)+sel.archived > 0 {
			what += fmt.Sprintf(" (%d observation(s), %d fact(s) already forgotten: --purge deletes them)", len(sel.obs), sel.archived)
		}
		return res, &output.Error{Code: output.Usage, Message: what}
	}
	docs, undeletable, err := purgeDocuments(ctx, cl, bank, sel.facts)
	if err != nil {
		return res, blocked(err)
	}
	for i := range docs {
		docs[i].Selected = selected[docs[i].ID]
	}
	res.Documents = docs
	if f.purge {
		res.Undeletable = undeletable
	}
	if f.plan {
		return res, nil
	}
	if !confirmed {
		what := "retires every memory of project " + f.project + " (reversible, one by one: backsight forget --restore <id>)"
		if f.purge {
			what = "deletes for good the documents listed (every memory extracted from them) and the observations of project " + f.project
		}
		if f.source != "" {
			what += ", source " + f.source + " only"
		}
		return res, &output.Error{Code: output.NeedsUser, Resume: f.resume() + " --answer id=value",
			Questions: []setup.Question{{ID: "confirm_project", Required: true, Free: true,
				Prompt: "this " + what + ", in bank " + bank + ": type the project name to confirm"}},
			Message: "forget --project needs answers to confirm_project (--answer confirm_project=<project name>): nothing was changed"}
	}
	if !f.purge {
		return forgetProjectFacts(ctx, cl, bank, f, sel, res)
	}
	return purgeProject(ctx, cl, bank, f, sel, res)
}

// selectProject lists the raw facts tagged project:P (and source:S when
// given) and the observations of P (with a source, only those drawn from
// the selected facts). The archive is listed too: counted for a plain
// forget, purged with --purge.
func selectProject(ctx context.Context, cl *hindsight.Client, bank, project, source string, purge bool) (projectSelection, error) {
	sel := projectSelection{obs: map[string]bool{}}
	tag := "project:" + project
	ids := map[string]bool{}
	for _, st := range []string{"", "invalidated"} {
		for _, ft := range []string{"world", "experience"} {
			err := cl.EachTagged(ctx, bank, ft, st, tag, func(m hindsight.Memory) error {
				if source != "" && !slices.Contains(m.Tags, "source:"+source) {
					return nil
				}
				if st != "" && !purge {
					sel.archived++
					return nil
				}
				ids[m.ID] = true
				sel.facts = append(sel.facts, hindsight.MemoryDetail{ID: m.ID, Type: ft, Tags: m.Tags, DocumentID: m.DocumentID, State: st})
				return nil
			})
			if err != nil {
				return sel, fmt.Errorf("listing %s facts of %s: %w", ft, project, err)
			}
		}
	}
	err := cl.EachTagged(ctx, bank, "observation", "", tag, func(m hindsight.Memory) error {
		if source != "" && !slices.ContainsFunc(m.SourceMemoryIDs, func(id string) bool { return ids[id] }) {
			return nil
		}
		sel.obs[m.ID] = true
		sel.obsTags = append(sel.obsTags, m.Tags)
		return nil
	})
	if err != nil {
		return sel, fmt.Errorf("listing the observations of %s: %w", project, err)
	}
	return sel, nil
}

// forgetProjectFacts retires every selected raw fact, as forget <id> does;
// the observations drawn from them regenerate without them. A fact gone
// since the listing is skipped; a failure stops the run, which the same
// command resumes (it lists only the live facts again).
func forgetProjectFacts(ctx context.Context, cl *hindsight.Client, bank string, f forgetProjectFlags, sel projectSelection, res forgetProjectResult) (forgetProjectResult, error) {
	reason := "forgotten by the user (backsight forget --project " + f.project + ")"
	for _, m := range sel.facts {
		var apiErr *hindsight.APIError
		switch err := cl.Invalidate(ctx, bank, m.ID, reason, false); {
		case err == nil:
			res.Retired++
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		default:
			return res, &output.Error{Code: output.Partial, Resume: f.resume(),
				Message: fmt.Sprintf("retiring %s: %v; %d of %d facts retired: run the same command again to finish", m.ID, err, res.Retired, len(sel.facts))}
		}
	}
	res.Action = "forgotten"
	return res, nil
}

// purgeProject deletes the selection as forget <id> --purge does, under
// the maintenance lock, then checks that the selected observations are
// gone: one drawn only from facts already archived has no live fact to
// clear it through.
func purgeProject(ctx context.Context, cl *hindsight.Client, bank string, f forgetProjectFlags, sel projectSelection, res forgetProjectResult) (forgetProjectResult, error) {
	tagged := append([]hindsight.MemoryDetail{}, sel.facts...)
	for _, t := range sel.obsTags {
		tagged = append(tagged, hindsight.MemoryDetail{Tags: t})
	}
	projects := projectsOf(tagged)
	projects[f.project] = true
	pages, mirror, err := purgeWrite(ctx, cl, bank, sel.facts, res.Documents, projects, "run the same command again to finish")
	res.Pages = pages
	if err != nil {
		var oe *output.Error
		if errors.As(err, &oe) {
			return res, err
		}
		return res, &output.Error{Code: output.Partial, Resume: f.resume(), Message: err.Error()}
	}
	err = cl.EachTagged(ctx, bank, "observation", "", "project:"+f.project, func(m hindsight.Memory) error {
		if sel.obs[m.ID] {
			res.ObservationsLeft = append(res.ObservationsLeft, m.ID)
		}
		return nil
	})
	if err != nil {
		return res, &output.Error{Code: output.Partial, Resume: f.resume(), Message: "purged, but checking the observations left failed: " + err.Error()}
	}
	res.Action = "purged"
	res.Note = "backups taken before now still hold it: delete them, or keep them out of reach, if the text was a secret"
	res.Backups = backupsBefore(now())
	if len(res.Undeletable) > 0 || len(res.ObservationsLeft) > 0 {
		res.Action = "partly_purged"
		var left []string
		if len(res.Undeletable) > 0 {
			left = append(left, "the facts with no document are retired but stay in Hindsight's archive (no API deletes them)")
		}
		if len(res.ObservationsLeft) > 0 {
			left = append(left, "the observations left were drawn only from facts already forgotten, which no longer clear them")
		}
		res.Note = strings.Join(left, "; ") + "; " + res.Note
	}
	if mirror != "" {
		res.Note += "; " + mirror
	}
	return res, nil
}

func forgetProjectText(r forgetProjectResult) string {
	var b strings.Builder
	scope := "project " + safeProject(r.Project)
	if r.Source != "" {
		scope += ", source " + r.Source
	}
	if r.Bank != "" {
		scope += " (bank " + r.Bank + ")"
	}
	m := r.Memories
	purge := r.Action != "forget_planned" && r.Action != "forgotten"
	fmt.Fprintf(&b, "%s: %d world, %d experience, %d observation(s); %d already forgotten%s\n", scope, m.World, m.Experience, m.Observation, m.Forgotten,
		map[bool]string{true: " (purged too)", false: ""}[purge && m.Forgotten > 0])
	if len(r.Documents) > 0 {
		head := map[bool]string{true: "documents (deleted with every memory extracted from them):", false: "documents (kept: a plain forget deletes nothing):"}[purge]
		b.WriteString(head + "\n")
		for _, d := range r.Documents {
			fmt.Fprintf(&b, "- %s: %d memories, %d selected\n", d.ID, d.Memories, d.Selected)
		}
	}
	switch r.Action {
	case "forget_planned":
		fmt.Fprintf(&b, "would retire the %d raw facts (reversible); the observations drawn from them regenerate without them\n", m.World+m.Experience)
	case "purge_planned":
		b.WriteString("would delete the documents listed and the observations, under the maintenance lock\n")
	case "forgotten":
		fmt.Fprintf(&b, "retired %d raw facts: out of recall and consolidation (undo one: backsight forget --restore <id>)\n", r.Retired)
	default:
		b.WriteString("deleted the documents listed and the observations\n")
	}
	if len(r.Undeletable) > 0 {
		fmt.Fprintf(&b, "not deletable (no document, retired only): %d fact(s)\n", len(r.Undeletable))
	}
	if len(r.ObservationsLeft) > 0 {
		fmt.Fprintf(&b, "observations left: %s\n", strings.Join(r.ObservationsLeft, ", "))
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
