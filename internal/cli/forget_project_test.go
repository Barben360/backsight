package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// bulkBank fronts the fake server with a bank that behaves like Hindsight
// 1.1.0 for forget --project: the listing honours type, state, tags
// (any_strict) and paging; a retired fact moves to the archive; clearing a
// fact's observations deletes those drawn from it; deleting a document
// deletes its facts, live or archived. Briefing page requests go on to the
// fake server.
type bulkBank struct {
	mu            sync.Mutex
	live, archive []bulkMem
	writes        []string // METHOD path body, in order
	banks         map[string]bool
	failPatch     string // a PATCH of this memory answers 500
	lockHeld      int    // writes made while forget --purge held the maintenance lock
	lockPath      string
}

type bulkMem struct {
	ID, Type, Doc string
	Tags, Sources []string
}

func (b *bulkBank) add(archived bool, id, typ, doc string, tags []string, sources ...string) {
	m := bulkMem{ID: id, Type: typ, Doc: doc, Tags: tags, Sources: sources}
	if archived {
		b.archive = append(b.archive, m)
	} else {
		b.live = append(b.live, m)
	}
}

func (b *bulkBank) ids(list []bulkMem) []string {
	var out []string
	for _, m := range list {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

func newBulkBank(t *testing.T, home string) *bulkBank {
	t.Helper()
	b := &bulkBank{banks: map[string]bool{}, lockPath: pathsOf(home).MaintenanceLock()}
	pointAt(t, home, proxyHindsight(t, fakeHindsight(t), b.answer))
	return b
}

func (b *bulkBank) answer(w http.ResponseWriter, r *http.Request) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/default/banks/")
	if !ok {
		return false
	}
	bank, sub, _ := strings.Cut(rest, "/")
	b.banks[bank] = true
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // forwarded when not answered here
	if r.Method != http.MethodGet {
		b.writes = append(b.writes, r.Method+" /"+sub+" "+string(body))
		if h := state.Inspect(b.lockPath); h != nil && !h.Stale && h.Owner.Command == "forget --purge" {
			b.lockHeld++
		}
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && sub == "memories/list":
		if q.Has("tags") && q.Get("tags_match") != "any_strict" {
			http.Error(w, "the selection must leave untagged memories out", http.StatusBadRequest)
			return true
		}
		from, st := b.live, "valid"
		if q.Get("state") == "invalidated" {
			from, st = b.archive, "invalidated"
		}
		var match []bulkMem
		for _, m := range from {
			if m.Type == q.Get("type") && (!q.Has("tags") || slices.Contains(m.Tags, q.Get("tags"))) {
				match = append(match, m)
			}
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		lim, _ := strconv.Atoi(q.Get("limit"))
		var items []string
		for i := off; i < min(off+lim, len(match)); i++ {
			m := match[i]
			tags, _ := json.Marshal(m.Tags)
			src, _ := json.Marshal(append([]string{}, m.Sources...))
			items = append(items, fmt.Sprintf(`{"id":%q,"text":"t","fact_type":%q,"document_id":%q,"state":%q,"tags":%s,"source_memory_ids":%s}`, m.ID, m.Type, m.Doc, st, tags, src))
		}
		fmt.Fprintf(w, `{"items":[%s],"total":%d}`, strings.Join(items, ","), len(match))
	case strings.HasPrefix(sub, "documents/"):
		doc := strings.TrimPrefix(sub, "documents/")
		n := 0
		for _, m := range append(append([]bulkMem{}, b.live...), b.archive...) {
			if m.Doc == doc {
				n++
			}
		}
		if n == 0 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if r.Method == http.MethodDelete {
			keep := func(m bulkMem) bool { return m.Doc == doc }
			b.live, b.archive = slices.DeleteFunc(b.live, keep), slices.DeleteFunc(b.archive, keep)
		}
		fmt.Fprintf(w, `{"id":%q,"memory_unit_count":%d}`, doc, n)
	case strings.HasPrefix(sub, "memories/") && strings.HasSuffix(sub, "/observations") && r.Method == http.MethodDelete:
		id := strings.TrimSuffix(strings.TrimPrefix(sub, "memories/"), "/observations")
		b.live = slices.DeleteFunc(b.live, func(m bulkMem) bool { return m.Type == "observation" && slices.Contains(m.Sources, id) })
		_, _ = w.Write([]byte(`{"deleted_count":1}`))
	case strings.HasPrefix(sub, "memories/") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(sub, "memories/")
		i := slices.IndexFunc(b.live, func(m bulkMem) bool { return m.ID == id })
		switch {
		case id == b.failPatch:
			w.WriteHeader(http.StatusInternalServerError)
		case i < 0:
			w.WriteHeader(http.StatusNotFound)
		case b.live[i].Type == "observation":
			w.WriteHeader(http.StatusBadRequest)
		default:
			b.archive = append(b.archive, b.live[i])
			b.live = slices.Delete(b.live, i, i+1)
			_, _ = w.Write([]byte(`{}`))
		}
	default:
		return false // briefing pages: the fake server
	}
	return true
}

func (b *bulkBank) writesMatching(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, w := range b.writes {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

type forgetProjectEnvelope struct {
	OK    bool                `json:"ok"`
	Data  forgetProjectResult `json:"data"`
	Error *struct {
		Code      int              `json:"code"`
		Message   string           `json:"message"`
		Questions []setup.Question `json:"questions"`
		Resume    string           `json:"resume"`
	} `json:"error"`
}

func forgetProject(t *testing.T, args ...string) (int, forgetProjectEnvelope, string) {
	t.Helper()
	code, out, errOut := run(t, append([]string{"forget", "--json"}, args...)...)
	var env forgetProjectEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("exit %d, not one envelope: %v\n%s\n%s", code, err, out, errOut)
	}
	return code, env, out
}

var (
	billing = []string{"project:billing", "source:chat"}
	infra   = []string{"project:infra", "source:chat"}
)

// The plan pages through every listing (more than one page of facts and
// of observations) and counts the selection by type; it writes nothing.
func TestForgetProjectPlanCountsAcrossPages(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	for i := range 450 {
		b.add(false, fmt.Sprintf("w%d", i), "world", fmt.Sprintf("conversation:b%d", i%3), billing)
	}
	for i := range 30 {
		b.add(false, fmt.Sprintf("e%d", i), "experience", "conversation:b0", billing)
	}
	for i := range 250 {
		b.add(false, fmt.Sprintf("o%d", i), "observation", "", billing, fmt.Sprintf("w%d", i))
	}
	for i := range 5 {
		b.add(true, fmt.Sprintf("z%d", i), "world", "conversation:b9", billing)
	}
	b.add(false, "i1", "world", "conversation:b0", infra) // another project's fact in a shared document
	b.add(false, "io", "observation", "", infra, "i1")

	code, env, out := forgetProject(t, "--project", "billing", "--plan")
	if code != output.OK || env.Data.Action != "forget_planned" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if want := (memoryCounts{World: 450, Experience: 30, Observation: 250, Forgotten: 5}); env.Data.Memories != want {
		t.Errorf("counts %+v, want %+v", env.Data.Memories, want)
	}
	// b0 holds 150 world, 30 experience and infra's fact: the plan says the
	// document holds more than the selection
	want := []purgeDoc{{ID: "conversation:b0", Memories: 181, Selected: 180}, {ID: "conversation:b1", Memories: 150, Selected: 150}, {ID: "conversation:b2", Memories: 150, Selected: 150}}
	if !reflect.DeepEqual(env.Data.Documents, want) {
		t.Errorf("documents %+v", env.Data.Documents)
	}
	code, env, out = forgetProject(t, "--project", "billing", "--purge", "--plan")
	if want := (memoryCounts{World: 450, Experience: 30, Observation: 250, Forgotten: 5}); code != output.OK || env.Data.Action != "purge_planned" || env.Data.Memories != want ||
		len(env.Data.Documents) != 4 || env.Data.Documents[3] != (purgeDoc{ID: "conversation:b9", Memories: 5, Selected: 5}) {
		t.Errorf("purge plan: exit %d\n%s", code, out)
	}
	if len(b.writes) != 0 || len(b.live) != 732 || len(b.archive) != 5 {
		t.Errorf("the plan wrote: %q", b.writes)
	}
	if code, out, _ := run(t, "forget", "--project", "billing", "--plan"); code != output.OK ||
		!strings.Contains(out, "project billing (bank claude-memory): 450 world, 30 experience, 250 observation(s); 5 already forgotten") ||
		!strings.Contains(out, "- conversation:b0: 181 memories, 180 selected") {
		t.Errorf("text plan: exit %d\n%s", code, out)
	}
}

// The write needs the project name typed back: without it the plan comes
// with a needs_user question, with a wrong one a usage error; neither
// writes anything.
func TestForgetProjectNeedsTheProjectName(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(false, "f1", "world", "conversation:s1", billing)
	for _, purge := range []bool{false, true} {
		args := []string{"--project", "billing"}
		if purge {
			args = append(args, "--purge")
		}
		code, env, out := forgetProject(t, args...)
		if code != output.NeedsUser || env.Error == nil || len(env.Error.Questions) != 1 || env.Error.Questions[0].ID != "confirm_project" ||
			!env.Error.Questions[0].Required || env.Data.Memories.World != 1 || len(env.Data.Documents) != 1 {
			t.Fatalf("no name (purge %v): exit %d\n%s", purge, code, out)
		}
		if want := "backsight " + strings.Join(append([]string{"forget"}, args...), " ") + " --answer id=value"; env.Error.Resume != want {
			t.Errorf("resume %q, want %q", env.Error.Resume, want)
		}
		code, env, out = forgetProject(t, append(args, "--answer", "confirm_project=infra")...)
		if code != output.Usage || env.Error == nil || !strings.Contains(env.Error.Message, "nothing was changed") {
			t.Errorf("wrong name (purge %v): exit %d\n%s", purge, code, out)
		}
	}
	if code, _, out := forgetProject(t, "--project", "billing", "--answer", "confirm_bank=billing"); code != output.Usage {
		t.Errorf("unknown answer: exit %d\n%s", code, out)
	}
	if len(b.writes) != 0 || len(b.live) != 1 {
		t.Errorf("wrote without the name: %q", b.writes)
	}
}

// A plain forget retires every live raw fact of the project, as forget
// <id> does, and nothing of another project; the archive is left alone.
func TestForgetProjectRetiresItsRawFacts(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(false, "f1", "world", "conversation:s1", billing)
	b.add(false, "f2", "world", "conversation:s1", billing)
	b.add(false, "e1", "experience", "conversation:s2", billing)
	b.add(false, "o1", "observation", "", billing, "f1", "f2")
	b.add(true, "z1", "world", "conversation:s0", billing)
	b.add(false, "i1", "world", "conversation:i1", infra)
	code, env, out := forgetProject(t, "--project", "billing", "--answer", "confirm_project=billing")
	if code != output.OK || env.Data.Action != "forgotten" || env.Data.Retired != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	reason := `{"reason":"forgotten by the user (backsight forget --project billing)","state":"invalidated"}`
	want := []string{"PATCH /memories/f1 " + reason, "PATCH /memories/f2 " + reason, "PATCH /memories/e1 " + reason}
	if !reflect.DeepEqual(b.writes, want) {
		t.Errorf("writes:\n%s", strings.Join(b.writes, "\n"))
	}
	if got := b.ids(b.archive); !reflect.DeepEqual(got, []string{"e1", "f1", "f2", "z1"}) {
		t.Errorf("archive %q", got)
	}
	// nothing live is left to forget: the rerun says so and writes nothing
	b.writes = nil
	if code, env, out := forgetProject(t, "--project", "billing", "--answer", "confirm_project=billing"); code != output.Usage ||
		!strings.Contains(env.Error.Message, "4 fact(s) already forgotten: --purge deletes them") || len(b.writes) != 0 {
		t.Errorf("rerun: exit %d\n%s", code, out)
	}
	if code, env, _ := forgetProject(t, "--project", "nope", "--plan"); code != output.Usage || !strings.Contains(env.Error.Message, "no live raw fact of project nope") {
		t.Errorf("unknown project: exit %d", code)
	}
}

// A run cut short says how far it went and resumes with the same command.
func TestForgetProjectCutShortIsResumable(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(false, "f1", "world", "conversation:s1", billing)
	b.add(false, "f2", "world", "conversation:s1", billing)
	b.failPatch = "f2"
	code, env, out := forgetProject(t, "--project", "billing", "--answer", "confirm_project=billing")
	if code != output.Partial || env.Data.Retired != 1 || env.Error.Resume != "backsight forget --project billing --answer confirm_project=billing" ||
		!strings.Contains(env.Error.Message, "1 of 2 facts retired") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	b.failPatch = ""
	if code, env, out := forgetProject(t, "--project", "billing", "--answer", "confirm_project=billing"); code != output.OK || env.Data.Retired != 1 || len(b.live) != 0 {
		t.Errorf("resume: exit %d\n%s", code, out)
	}
}

// A purge deletes the documents of the project's raw facts, archived ones
// included, clears its observations first, under the maintenance lock, and
// leaves another project's memories, documents and briefing page alone.
func TestForgetProjectPurgeDeletesOnlyThatProject(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(false, "f1", "world", "conversation:s1", billing)
	b.add(false, "e1", "experience", "conversation:s2", billing)
	b.add(true, "z1", "world", "conversation:s3", billing)
	b.add(false, "o1", "observation", "", billing, "f1")
	b.add(false, "o2", "observation", "", billing, "e1", "f1")
	b.add(false, "i1", "world", "conversation:i1", infra)
	b.add(false, "io", "observation", "", infra, "i1")
	for _, p := range []string{"billing", "infra"} {
		id := briefingModelID(p)
		mentalModels[id] = `{"id":"` + id + `","content":"x","tags":["project:` + p + `"]}`
	}
	code, env, out := forgetProject(t, "--project", "billing", "--purge", "--answer", "confirm_project=billing")
	if code != output.OK || env.Data.Action != "purged" || len(env.Data.ObservationsLeft) != 0 || !reflect.DeepEqual(env.Data.Pages, []string{briefingModelID("billing")}) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if live, archive := b.ids(b.live), b.ids(b.archive); !reflect.DeepEqual(live, []string{"i1", "io"}) || len(archive) != 0 {
		t.Errorf("left: live %q, archive %q", live, archive)
	}
	docs := b.writesMatching("DELETE /documents/")
	if want := []string{"DELETE /documents/conversation:s1 ", "DELETE /documents/conversation:s2 ", "DELETE /documents/conversation:s3 "}; !reflect.DeepEqual(docs, want) {
		t.Errorf("documents deleted %q", docs)
	}
	// every fact is cleared and retired before the first document goes
	firstDoc := slices.IndexFunc(b.writes, func(w string) bool { return strings.HasPrefix(w, "DELETE /documents/") })
	if n := len(b.writesMatching("DELETE /memories/")) + len(b.writesMatching("PATCH /memories/")); n != 4 || firstDoc != 4 {
		t.Errorf("writes:\n%s", strings.Join(b.writes, "\n"))
	}
	if _, kept := mentalModels[briefingModelID("infra")]; !kept {
		t.Error("another project's briefing page was deleted")
	}
	if b.lockHeld != len(b.writes) {
		t.Errorf("%d of %d writes under the maintenance lock", b.lockHeld, len(b.writes))
	}
	// besides the memory bank, only the control bank, for the tombstones
	if !reflect.DeepEqual(b.banks, map[string]bool{"claude-memory": true, "claude-memory-control": true}) {
		t.Errorf("banks touched: %v", b.banks)
	}
	last := b.writes[len(b.writes)-1]
	for _, want := range []string{"POST /memories ", `"document_id":"backsight:tombstones"`, `conversation:s1`, `conversation:s2`, `conversation:s3`} {
		if !strings.Contains(last, want) {
			t.Errorf("the last write is not the tombstones of the three documents: %s", last)
		}
	}
	if strings.Contains(last, "conversation:i1") {
		t.Errorf("another project's document got a tombstone: %s", last)
	}
}

// --source narrows the raw facts to that source and the observations to
// those drawn from them, in the plan, a plain forget and a purge.
func TestForgetProjectSourceNarrows(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	sub := []string{"project:billing", "source:subagent"}
	b.add(false, "c1", "world", "conversation:c", billing)
	b.add(false, "s1", "world", "subagent:a", sub)
	b.add(false, "s2", "experience", "subagent:a", sub)
	b.add(false, "oc", "observation", "", billing, "c1")
	b.add(false, "os", "observation", "", billing, "s1")
	b.add(false, "om", "observation", "", billing, "c1", "s2") // drawn from both
	b.add(false, "x1", "world", "subagent:x", []string{"project:infra", "source:subagent"})

	code, env, out := forgetProject(t, "--project", "billing", "--source", "subagent", "--plan")
	if want := (memoryCounts{World: 1, Experience: 1, Observation: 2}); code != output.OK || env.Data.Memories != want ||
		!reflect.DeepEqual(env.Data.Documents, []purgeDoc{{ID: "subagent:a", Memories: 2, Selected: 2}}) {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	code, _, out = forgetProject(t, "--project", "billing", "--source", "subagent", "--answer", "confirm_project=billing")
	if got := b.writesMatching(""); code != output.OK || len(got) != 2 || !strings.HasPrefix(got[0], "PATCH /memories/s1 ") || !strings.HasPrefix(got[1], "PATCH /memories/s2 ") {
		t.Fatalf("forget: exit %d, writes %q\n%s", code, got, out)
	}
	// restore them, then purge the source
	b.live = append(b.live, b.archive...)
	b.archive, b.writes = nil, nil
	code, env, out = forgetProject(t, "--project", "billing", "--source", "subagent", "--purge", "--answer", "confirm_project=billing")
	if code != output.OK || env.Data.Action != "purged" {
		t.Fatalf("purge: exit %d\n%s", code, out)
	}
	if got := b.ids(b.live); !reflect.DeepEqual(got, []string{"c1", "oc", "x1"}) {
		t.Errorf("live after the purge %q", got)
	}
	if got := b.writesMatching("DELETE /documents/"); !reflect.DeepEqual(got, []string{"DELETE /documents/subagent:a "}) {
		t.Errorf("documents deleted %q", got)
	}
}

// An observation drawn only from facts already forgotten has no live fact
// to clear it through: the purge says it is left instead of claiming it.
func TestForgetProjectPurgeSaysWhichObservationsAreLeft(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(true, "z1", "world", "conversation:s1", billing)
	b.add(false, "o9", "observation", "", billing, "z1")
	code, env, out := forgetProject(t, "--project", "billing", "--purge", "--answer", "confirm_project=billing")
	if code != output.OK || env.Data.Action != "partly_purged" || !reflect.DeepEqual(env.Data.ObservationsLeft, []string{"o9"}) {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestForgetProjectExcludesTheIDForm(t *testing.T) {
	home := devInstall(t, true)
	b := newBulkBank(t, home)
	b.add(false, "f1", "world", "conversation:s1", billing)
	for _, args := range [][]string{
		{"forget", "f1", "--project", "billing"},
		{"forget", "--project", "billing", "--restore"},
		{"forget", "--project", "billing", "--purge", "--yes"},
		{"forget", "--project", ""},
		{"forget", "f1", "--source", "chat"},
		{"forget", "f1", "--answer", "confirm_project=billing"},
		{"forget"},
	} {
		if code, _, _ := run(t, args...); code != output.Usage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	if len(b.writes) != 0 {
		t.Errorf("writes %q", b.writes)
	}
}
