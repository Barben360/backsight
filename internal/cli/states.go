package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/staleref"
)

// memory states (issue 331): the `state` facts that say a GitHub pull
// request or issue is still open, checked against GitHub. Deterministic
// (gh, no LLM) and run by the user: --retire invalidates the ones GitHub
// contradicts, as forget does, reversible with forget --restore.

// stateRef is a pull request or issue a fact names, as GitHub has it.
type stateRef struct {
	Ref   string    `json:"ref"`   // owner/repo#n
	State string    `json:"state"` // merged | closed | open
	At    time.Time `json:"at,omitzero"`
	URL   string    `json:"url,omitempty"`
}

// stateFact is a fact GitHub contradicts.
type stateFact struct {
	ID      string     `json:"id"`
	Project string     `json:"project,omitempty"`
	Text    string     `json:"text"` // redacted, one line, clipped
	Refs    []stateRef `json:"refs"`
	Command string     `json:"command"`
}

type statesResult struct {
	Read      int            `json:"read"`       // live state facts read
	Checked   int            `json:"checked"`    // of those, saying a pull request or issue is still open
	Outdated  []stateFact    `json:"outdated"`   // GitHub says every item it names merged or closed
	StillOpen int            `json:"still_open"` // GitHub says one of its items is still open
	Skipped   map[string]int `json:"skipped"`    // why a checked fact was not decided → how many
	Retired   []string       `json:"retired,omitempty"`
}

// Reasons a fact saying an item is still open is left undecided.
const (
	skipSaysDone       = "says_done"            // it also says the item was merged or closed since
	skipNoRepository   = "no_repository"        // a bare ref, and no project to find its repository by
	skipUnknownName    = "unknown_name"         // a bare ref after a name that is no known project
	skipSeveral        = "several_projects"     // a bare ref, and the text names several projects
	skipOtherProject   = "other_project"        // a bare ref, and the text names a project not the fact's own
	skipClosedUnmerged = "closed_unmerged"      // a pull request closed without a merge: "not merged" may be true
	skipMixedKind      = "mixed_kind"           // also a decision, convention, pitfall…: retiring it would lose that part
	skipNewerItem      = "newer_item"           // GitHub's item under that number is newer than the fact: a recreated repository
	skipNotFound       = "not_found"            // no such item under the user's owners
	skipAmbiguous      = "several_repositories" // the item exists under several owners
	skipGitHubError    = "github_error"         // GitHub did not answer
	statesTextMax      = 200
)

var skipWords = map[string]string{
	skipSaysDone:       "also says it was done since",
	skipNoRepository:   "no project to find the repository by",
	skipUnknownName:    "names a repository that is no known project",
	skipSeveral:        "names several projects",
	skipOtherProject:   "names a project other than its own",
	skipClosedUnmerged: "a pull request closed without a merge",
	skipMixedKind:      "also holds a decision, convention, pitfall or correction",
	skipNewerItem:      "the item under that number is newer than the memory (a recreated repository)",
	skipNotFound:       "not found on GitHub under your owners",
	skipAmbiguous:      "found under several owners",
	skipGitHubError:    "GitHub did not answer",
}

// ghItem is what GitHub's issues endpoint says of an issue or a pull
// request (the endpoint serves both).
type ghItem struct {
	State       string    `json:"state"` // open | closed
	CreatedAt   time.Time `json:"created_at"`
	ClosedAt    time.Time `json:"closed_at"`
	HTMLURL     string    `json:"html_url"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

var (
	errGHNotFound = errors.New("not found on GitHub")
	errGHAuth     = errors.New("gh is not logged in to GitHub")
)

// ghTimeout bounds one gh call: a stuck one fails that item, not the run.
const ghTimeout = 30 * time.Second

// ghLookPath and ghIssue reach GitHub through the gh CLI (its login, so
// private repositories too); variables so tests answer for GitHub.
var (
	ghLookPath = exec.LookPath
	ghIssue    = func(ctx context.Context, owner, repo string, n int) (ghItem, error) {
		bin, err := ghLookPath("gh")
		if err != nil {
			return ghItem{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, ghTimeout)
		defer cancel()
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, "api", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, n))
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			s := stderr.String()
			switch {
			case strings.Contains(s, "HTTP 404"):
				return ghItem{}, errGHNotFound
			case strings.Contains(s, "HTTP 401") || strings.Contains(s, "gh auth login"):
				return ghItem{}, errGHAuth
			}
			line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
			return ghItem{}, fmt.Errorf("gh api: %s", clip(line, 200))
		}
		var it ghItem
		if err := json.Unmarshal(out, &it); err != nil || it.State == "" {
			return ghItem{}, fmt.Errorf("gh api: unexpected answer for %s/%s#%d", owner, repo, n)
		}
		return it, nil
	}
)

func newMemoryStatesCmd(e *env) *cobra.Command {
	var retire bool
	var proj string
	cmd := &cobra.Command{
		Use:   "states",
		Short: "Check the memories saying a GitHub pull request or issue is still open against GitHub, and list those it contradicts",
		Long: "Lists the delivered-state memories that name a GitHub pull request or issue and say it is still open (open, not merged, " +
			"waiting for a review), checks each item with the gh CLI, and shows those GitHub says were merged or closed since, " +
			"with the command that retires each. --retire retires them all; backsight forget --restore <id> undoes one. " +
			"A bare \"PR #12\" is looked up under your own repository owners, in the project the memory names.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := ghLookPath("gh"); err != nil {
				return &output.Error{Code: output.NeedsUser, Message: "memory states checks GitHub with the gh CLI, which is not installed",
					Actions: []output.Action{{Kind: "install", Cmd: "gh auth login", Why: "install gh (https://cli.github.com), log in, then run backsight memory states again"}}}
			}
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			var cl *hindsight.Client
			var bank string
			gate := func() error { return nil }
			if retire {
				if err := heldWrites(); err != nil {
					return err
				}
				cl, bank, gate, err = curateWriter()
			} else {
				cl, bank, err = curateClient()
			}
			if err != nil {
				return err
			}
			if proj != "" {
				proj = canonicalProject(cfg, proj)
			}
			ctx := cmd.Context()
			var projects []string
			tags, err := cl.Tags(ctx, bank, "project:")
			if err != nil {
				return blocked(err)
			}
			for _, t := range tags {
				projects = append(projects, strings.TrimPrefix(t.Tag, "project:"))
			}
			owners := githubOwners((retain.Attributor{Dir: d.AttributionDir(), Owners: cfg.Projects.OwnOwners, Deny: cfg.Projects.DenyOwners}).OwnOwners())
			var facts []hindsight.Memory
			for _, typ := range []string{"world", "experience"} {
				if err := cl.EachTagged(ctx, bank, typ, "", "kind:state", func(m hindsight.Memory) error {
					if slices.Contains(m.Tags, "kind:state") && (proj == "" || recall.ProjectOf(m.Tags) == proj) {
						facts = append(facts, m)
					}
					return nil
				}); err != nil {
					return blocked(err)
				}
			}
			res, err := checkStates(ctx, facts, projects, owners)
			if errors.Is(err, errGHAuth) {
				return &output.Error{Code: output.NeedsUser, Message: "gh is not logged in to GitHub: nothing was checked",
					Actions: []output.Action{{Kind: "relogin", Cmd: "gh auth login", Why: "memory states reads pull requests and issues through gh"}}}
			}
			if err != nil {
				return blocked(err)
			}
			if retire && len(res.Outdated) > 0 {
				if err := gate(); err != nil {
					return err
				}
				for _, f := range res.Outdated {
					if err := cl.Invalidate(ctx, bank, f.ID, retireReason(f), false); err != nil {
						journalForget(res.Retired)
						return e.report("memory states", res, func() string { return statesText(res, retire) },
							&output.Error{Code: output.Partial, Message: fmt.Sprintf("retired %d of %d, then: %v; run it again to retire the rest", len(res.Retired), len(res.Outdated), err)})
					}
					res.Retired = append(res.Retired, f.ID)
				}
				journalForget(res.Retired)
			}
			return e.emit("memory states", res, func() string { return statesText(res, retire) })
		},
	}
	cmd.Flags().BoolVar(&retire, "retire", false, "retire every memory listed (backsight forget --restore <id> undoes one)")
	cmd.Flags().StringVar(&proj, "project", "", `only this project's memories ("transverse" for the transverse pool)`)
	return cmd
}

// githubOwners are the github.com owners of owners ("github.com/acme"),
// lower case, each once.
func githubOwners(owners []string) []string {
	var out []string
	for _, o := range owners {
		o = strings.ToLower(o)
		if name, ok := strings.CutPrefix(o, "github.com/"); ok && name != "" && !strings.Contains(name, "/") && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// checkStates checks facts against GitHub; errGHAuth stops it.
func checkStates(ctx context.Context, facts []hindsight.Memory, projects, owners []string) (statesResult, error) {
	res := statesResult{Read: len(facts), Outdated: []stateFact{}, Skipped: map[string]int{}}
	cache := map[string]ghAnswer{}
	lookup := func(owner, repo string, n int) ghAnswer {
		key := fmt.Sprintf("%s/%s#%d", owner, repo, n)
		if a, ok := cache[key]; ok {
			return a
		}
		it, err := ghIssue(ctx, owner, repo, n)
		a := ghAnswer{ref: key, item: it, err: err}
		cache[key] = a
		return a
	}
	for _, m := range facts {
		refs := staleref.Refs(m.Text)
		if len(refs) == 0 || !staleref.ClaimsOpen(m.Text) {
			continue
		}
		res.Checked++
		if staleref.SaysDone(m.Text) {
			res.Skipped[skipSaysDone]++
			continue
		}
		// A fact that is also a decision, convention, pitfall, lesson or
		// correction keeps a durable part a retirement would lose.
		if kinds, _ := recall.KindsOf(m.Tags); !slices.Equal(kinds, []string{"state"}) {
			res.Skipped[skipMixedKind]++
			continue
		}
		project := recall.ProjectOf(m.Tags)
		var got []stateRef
		skip := ""
		for _, r := range refs {
			repos, why := refRepos(r, m.Text, project, projects, owners)
			if why != "" {
				skip = why
				break
			}
			var found []ghAnswer
			for _, or := range repos {
				a := lookup(or[0], or[1], r.Number)
				switch {
				case errors.Is(a.err, errGHAuth):
					return res, errGHAuth
				case errors.Is(a.err, errGHNotFound):
				case a.err != nil:
					skip = skipGitHubError
				default:
					found = append(found, a)
				}
			}
			switch {
			case skip != "":
			case len(found) == 0:
				skip = skipNotFound
			case len(found) > 1:
				skip = skipAmbiguous
			case newerThan(found[0].item, m):
				skip = skipNewerItem
			default:
				if s := found[0].stateRef(); !slices.ContainsFunc(got, func(g stateRef) bool { return g.Ref == s.Ref }) {
					got = append(got, s)
				}
			}
			if skip != "" {
				break
			}
		}
		switch {
		case skip != "":
			res.Skipped[skip]++
		case slices.ContainsFunc(got, func(s stateRef) bool { return s.State == "open" }):
			res.StillOpen++
		case slices.ContainsFunc(got, func(s stateRef) bool { return s.State == closedUnmerged }):
			res.Skipped[skipClosedUnmerged]++
		default:
			red, _ := redact.Text(m.Text)
			res.Outdated = append(res.Outdated, stateFact{ID: m.ID, Project: project, Text: clip(strings.Join(strings.Fields(red), " "), statesTextMax),
				Refs: got, Command: "backsight forget " + m.ID})
		}
	}
	return res, nil
}

// newerThan is whether GitHub's item was created after the fact was
// written (an hour of slack for the clocks): the fact cannot be about it,
// its repository was recreated and the number reused.
func newerThan(it ghItem, m hindsight.Memory) bool {
	when := m.When()
	return !when.IsZero() && !it.CreatedAt.IsZero() && it.CreatedAt.After(when.Add(time.Hour))
}

type ghAnswer struct {
	ref  string
	item ghItem
	err  error
}

func (a ghAnswer) stateRef() stateRef {
	s := stateRef{Ref: a.ref, State: a.item.State, URL: a.item.HTMLURL}
	if a.item.State == "closed" {
		s.At = a.item.ClosedAt
		switch pr := a.item.PullRequest; {
		case pr != nil && pr.MergedAt != nil:
			s.State, s.At = "merged", *pr.MergedAt
		case pr != nil:
			s.State = closedUnmerged
		}
	}
	return s
}

// closedUnmerged: a pull request closed without a merge, which a "not
// merged" fact may still describe truly; such a fact is skipped.
const closedUnmerged = "closed_unmerged"

// refRepos are the repositories (owner, repo) r may be in, or why it
// cannot be told: a URL's own; else, under every owner, the project named
// just before it, the one project the text names, or the fact's project.
func refRepos(r staleref.Ref, text, project string, projects, owners []string) ([][2]string, string) {
	if !r.Bare() {
		if r.Repo == "." || r.Repo == ".." {
			return nil, skipNotFound
		}
		return [][2]string{{r.Owner, r.Repo}}, ""
	}
	name := ""
	switch named := staleref.Names(text, projects); {
	case r.Qualifier != "" && slices.ContainsFunc(projects, func(p string) bool { return strings.EqualFold(p, r.Qualifier) }):
		name = r.Qualifier
	case r.Qualifier != "":
		return nil, skipUnknownName
	case len(named) > 1:
		return nil, skipSeveral
	case len(named) == 1 && project != "" && !strings.HasPrefix(project, ".") && named[0] != strings.ToLower(project):
		// a project named in passing ("waits for the hindsight release")
		// is no proof the item is in its repository
		return nil, skipOtherProject
	case len(named) == 1:
		name = named[0]
	case project != "" && !strings.HasPrefix(project, "."):
		name = strings.ToLower(project)
	default:
		return nil, skipNoRepository
	}
	if len(owners) == 0 {
		return nil, skipNoRepository
	}
	out := make([][2]string, 0, len(owners))
	for _, o := range owners {
		out = append(out, [2]string{o, name})
	}
	return out, ""
}

// retireReason is the invalidation reason, with GitHub's answer as the
// evidence.
func retireReason(f stateFact) string {
	var parts []string
	for _, r := range f.Refs {
		parts = append(parts, r.Ref+" "+r.State+" "+r.At.UTC().Format(time.RFC3339))
	}
	return "outdated (backsight memory states): GitHub says " + strings.Join(parts, ", ")
}

func statesText(r statesResult, retire bool) string {
	var b strings.Builder
	if len(r.Outdated) > 0 {
		verb := "GitHub says otherwise"
		switch {
		case len(r.Retired) == len(r.Outdated):
			verb = "GitHub says otherwise; retired"
		case len(r.Retired) > 0:
			verb = fmt.Sprintf("GitHub says otherwise; the first %d retired, the others not", len(r.Retired))
		}
		fmt.Fprintf(&b, "%d %s still open, and %s:\n", len(r.Outdated), forms(len(r.Outdated), "memory says an item is", "memories say an item is"), verb)
		for _, f := range r.Outdated {
			var refs []string
			for _, s := range f.Refs {
				refs = append(refs, s.Ref+" "+s.State+" "+s.At.UTC().Format("2006-01-02"))
			}
			fmt.Fprintf(&b, "%s  %s  %s\n  %s\n  %s\n", shortID(f.ID), safeProject(f.Project), strings.Join(refs, ", "), f.Text, f.Command)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Read %d state %s; %d %s a pull request or issue is still open: %d outdated, %d still open, %d skipped",
		r.Read, forms(r.Read, "memory", "memories"), r.Checked, forms(r.Checked, "says", "say"), len(r.Outdated), r.StillOpen, skippedTotal(r))
	if why := skippedWords(r); why != "" {
		b.WriteString(" (" + why + ")")
	}
	b.WriteString(".\n")
	switch {
	case len(r.Retired) > 0:
		fmt.Fprintf(&b, "Retired %d of %d; backsight forget --restore <id> undoes one.\n", len(r.Retired), len(r.Outdated))
	case len(r.Outdated) > 0 && !retire:
		b.WriteString("backsight memory states --retire retires them; backsight forget --restore <id> undoes one.\n")
	}
	return b.String()
}

func skippedTotal(r statesResult) int {
	n := 0
	for _, v := range r.Skipped {
		n += v
	}
	return n
}

func skippedWords(r statesResult) string {
	keys := make([]string, 0, len(r.Skipped))
	for k := range r.Skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", r.Skipped[k], skipWords[k]))
	}
	return strings.Join(parts, ", ")
}

// forms is one when n is 1, else many.
func forms(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
