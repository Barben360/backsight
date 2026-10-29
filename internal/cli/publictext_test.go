package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
)

// Text a user sees names nothing they cannot look up: the project's design
// records, experiments, milestones and spec sections are not published, so
// a "(decision 0032)" or an "(E7)" in a setup question, a --help line, an
// error, a doctor check or a generated file only puzzles. Each one is
// replaced by its reason in plain words. Code comments are not scanned.

var privateRefs = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a design-record number", regexp.MustCompile(`(?i)\b(?:decisions?|plan) \d{4}\b`)},
	{"a spec section", regexp.MustCompile(`\bSPEC\b|§`)},
	{"an experiment, review or milestone code", regexp.MustCompile(`\b[CEM]\d{1,2}\b`)},
	{"the roadmap or a review round", regexp.MustCompile(`(?i)\bROADMAP\b|\bblock review\b|\bsecurity review [A-Z]\d`)},
	{`"tranche"`, regexp.MustCompile(`(?i)\btranches?\b`)},
	{`"the reference" (an older Hindsight install)`, regexp.MustCompile(`(?i)\breference(?:'s)? (?:install|server|stack|config|pieces?|hindsight|retire|client|baseline)\b|\bthe reference(?:'s)?(?:[),;.:]|\s+stays\b|$)`)},
	{"the author's machine", regexp.MustCompile(`(?i)\bauthor'?s'? (?:machine|pc|computer|laptop|install)`)},
	{`"pre-image" (a backup copy)`, regexp.MustCompile(`(?i)\bpre-?images?\b`)},
	{"an issue number without its URL", regexp.MustCompile(`(?i)\bissues? #?\d+\b`)},
}

// privateRefsIn lists what text cites, "" when it cites nothing.
func privateRefsIn(text string) string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		for _, r := range privateRefs {
			for _, m := range r.re.FindAllString(line, -1) {
				found = append(found, fmt.Sprintf("%s %q", r.what, m))
			}
		}
	}
	return strings.Join(found, "; ")
}

func checkPublic(t *testing.T, where, text string) {
	t.Helper()
	if refs := privateRefsIn(text); refs != "" {
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		t.Errorf("%s names %s:\n%s", where, refs, text)
	}
}

func TestPrivateReferencePatterns(t *testing.T) {
	cited := []string{
		"usage counts against your plan (decision 0032)",
		"Backfill and rebuilds read them (decisions 0041 and 0043)",
		"Check for and install backsight releases (SPEC §11.7)",
		"untagged observations are invisible to project recall (§4)",
		"cheapest measured (E7): about $0.14",
		"compare with the reference stack's C12 baseline",
		"report it upstream (backsight report, M5)",
		"searched with every project (E30 V3)",
		"takes about 2 min per tranche on CPU",
		"DeepSeek API (the reference install)",
		"start the reference server",
		"server hosts (backsight's and the reference's)",
		"the reference stays a fallback until you stop it",
		"Tested on the author's machine with Docker 29.9",
		"with --undo: restore pre-images over files edited since",
		"see issue #281",
		"until then the reference stack stays (ROADMAP v7)",
		"the agent-driven install (plan 0063)",
	}
	for _, s := range cited {
		if privateRefsIn(s) == "" {
			t.Errorf("not caught: %q", s)
		}
	}
	plain := []string{
		"Docker Compose v2 is missing",
		"pinned by digest: hindsight:1.1.0@sha256:50311d02b4d5d2b7c5242bd336edf8735df8374eaad496a92f49c83b37d2aa06",
		"current key CB9E8D3622A9CEDE",
		"backsight_0.1.0-m5_linux_amd64.tar.gz",
		"3 pieces of an older Hindsight install wired into Claude Code (see the reference question)",
		"--answer pre_images_ack=yes",
		"known limit: https://github.com/Barben360/backsight/issues/5",
		"e2e tests and a 2 min E2E run",
		"keeps transcripts 3650 days",
	}
	for _, s := range plain {
		if refs := privateRefsIn(s); refs != "" {
			t.Errorf("%q flagged: %s", s, refs)
		}
	}
}

// Every command's help, hidden ones included: its use line, descriptions,
// examples and each flag's usage.
func TestHelpNamesNoPrivateReference(t *testing.T) {
	n := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		n++
		where := "backsight help of " + c.CommandPath()
		checkPublic(t, where, strings.Join([]string{c.Use, c.Short, c.Long, c.Example, c.Deprecated}, "\n"))
		for _, fs := range []*pflag.FlagSet{c.LocalFlags(), c.PersistentFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				checkPublic(t, where+" --"+f.Name, f.Usage+"\n"+f.Deprecated)
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRoot(&env{stdout: os.Stdout, stderr: os.Stderr}))
	if n < 60 {
		t.Fatalf("walked %d commands: want the whole tree (60 or more)", n)
	}
}

// Every question setup can ask, for each provider it lists and an unlisted
// one, with the questions that only some installs get (an older Hindsight
// install to retire, past sessions to backfill).
func TestSetupQuestionsNameNoPrivateReference(t *testing.T) {
	for _, p := range setup.Presets {
		checkPublic(t, "setup preset "+p.Provider, strings.Join([]string{p.Label, p.Cost, p.Caveat}, "\n"))
	}
	providers := []string{"groq"}
	for _, p := range setup.Presets {
		providers = append(providers, p.Provider)
	}
	for _, provider := range providers {
		qs, _, err := setup.Resolve(setup.Inputs{
			Answers:         map[string]string{"provider": provider},
			ReferencePieces: 2,
			Backfill:        func(string) (int, string) { return 12, "about $0.40" },
		})
		if err != nil {
			t.Fatalf("provider=%s: %v", provider, err)
		}
		ids := map[string]bool{}
		for _, q := range qs {
			ids[q.ID] = true
			checkPublic(t, "setup question "+q.ID+" (provider="+provider+")", q.Prompt)
			for _, o := range q.Options {
				checkPublic(t, "setup question "+q.ID+" option "+o.Value+" (provider="+provider+")", strings.Join([]string{o.Label, o.Cost, o.Caveat}, "\n"))
			}
		}
		for _, id := range []string{"provider", "retention", "reference", "backfill", "consent"} {
			if !ids[id] {
				t.Errorf("provider=%s: no %s question: the scan misses it", provider, id)
			}
		}
	}
}

// The files backsight writes for the user to read: the server's compose
// file and .env, and the scheduler entries of every system.
func TestGeneratedFilesNameNoPrivateReference(t *testing.T) {
	cfg := config.Default(paths.Dirs{Data: "/home/u/.local/share/backsight"}, "/home/u")
	f, err := server.Render(cfg, config.Secrets{APIKey: "a", UIKey: "b", DBPassword: "c", LLMAPIKey: "d"}, "box")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Compose) == 0 || len(f.Env) == 0 {
		t.Fatal("server render wrote an empty file: the scan proves nothing")
	}
	checkPublic(t, "the rendered compose.yaml", string(f.Compose))
	checkPublic(t, "the rendered .env", string(f.Env))
	for _, goos := range []string{"linux", "darwin", "windows"} {
		p, err := scheduler.For(goos, scheduler.Options{Shim: "/home/u/.local/bin/backsight", Home: "/home/u",
			UnitDir: "/home/u/.config/systemd/user", UID: 1000, LogDir: "/home/u/.local/state/backsight/logs", Env: map[string]string{"PATH": "/usr/bin"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Files)+len(p.Activate) == 0 {
			t.Fatalf("%s: no scheduler file or command: the scan proves nothing", goos)
		}
		for _, file := range p.Files {
			checkPublic(t, goos+" scheduler entry "+filepath.Base(file.Path), file.Content)
		}
		for _, c := range p.Activate { // schtasks takes the task's definition as arguments
			checkPublic(t, goos+" scheduler command", strings.Join(c, " "))
		}
	}
}

// Literals that match a pattern but never reach a user as text.
var notUserText = map[string]string{
	"internal/paths/paths.go\x00pre-images": "the directory's name on disk: renaming it would orphan the backup copies of existing installs",
	"internal/reference/reference.go\x00M4": "Piece.ReplacedIn, never printed",
}

// Every string literal of the program (test files, struct tags and import
// paths aside): errors, doctor's checks and fixes, setup's plan and step
// results, command output. Text built at run time from these parts is
// covered by the surfaces above.
func TestStringLiteralsNameNoPrivateReference(t *testing.T) {
	root := filepath.Join("..", "..")
	used := map[string]bool{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "dist" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		skip := map[ast.Node]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ImportSpec:
				skip[n.Path] = true
			case *ast.Field:
				if n.Tag != nil {
					skip[n.Tag] = true
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING || skip[n] {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err != nil {
					t.Errorf("%s: %v", fset.Position(n.Pos()), err)
					return true
				}
				if refs := privateRefsIn(s); refs != "" {
					if _, ok := notUserText[rel+"\x00"+s]; ok {
						used[rel+"\x00"+s] = true
						return true
					}
					t.Errorf("%s:%d names %s: %q", rel, fset.Position(n.Pos()).Line, refs, s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 120 {
		t.Fatalf("scanned %d Go files: want the whole module", files)
	}
	for k, why := range notUserText {
		if !used[k] {
			t.Errorf("notUserText lists %q (%s), which no longer exists: drop it", strings.ReplaceAll(k, "\x00", ": "), why)
		}
	}
}

// What a release publishes for its readers: the release notes, the
// documents they point to for versions and support, the README and every
// docs page (CONTRIBUTING.md is for contributors and stays out).
func TestReleaseDocumentsNameNoPrivateReference(t *testing.T) {
	notes, err := filepath.Glob(filepath.Join("..", "..", "docs", "releases", "*.md"))
	if err != nil || len(notes) == 0 {
		t.Fatalf("no release notes in docs/releases: %v", err)
	}
	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("no pages in docs: %v", err)
	}
	files := slices.Concat(notes, pages, []string{securityDoc, compatibilityDoc, filepath.Join("..", "..", "README.md")})
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checkPublic(t, filepath.Base(f), string(raw))
	}
}
