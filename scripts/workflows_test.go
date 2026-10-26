package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflow is one file of .github/workflows, cut along its YAML indent:
// the top-level env block, then each job and its steps.
type workflow struct {
	name string
	env  string // the top-level env: block, "" without one
	jobs []job
}

type job struct {
	name, raw string
	steps     []string // each block starting at a job's "      - "
}

// blocks cuts s at each match of start, every block running to the next.
func blocks(s string, start *regexp.Regexp) (out []string) {
	idx := start.FindAllStringIndex(s, -1)
	for i, at := range idx {
		end := len(s)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, s[at[0]:end])
	}
	return out
}

func workflows(t *testing.T) []workflow {
	t.Helper()
	files, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	var out []workflow
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		w := workflow{name: filepath.Base(f)}
		for _, s := range blocks(string(raw), regexp.MustCompile(`(?m)^[a-z]`)) {
			switch strings.SplitN(s, ":", 2)[0] {
			case "env":
				w.env = s
			case "jobs":
				for _, j := range blocks(s, regexp.MustCompile(`(?m)^  [a-z0-9_-]+:\s*$`)) {
					w.jobs = append(w.jobs, job{
						name:  strings.TrimSuffix(strings.TrimSpace(strings.SplitN(j, "\n", 2)[0]), ":"),
						raw:   j,
						steps: blocks(j, regexp.MustCompile(`(?m)^      - `)),
					})
				}
			}
		}
		if len(w.jobs) == 0 {
			t.Fatalf("%s: no job found: the checks would prove nothing", w.name)
		}
		out = append(out, w)
	}
	return out
}

// The CI rules of decision 0067's block review, which a workflow edit
// could silently drop.
func TestWorkflowsKeepTheirSafetyRules(t *testing.T) {
	pipe := regexp.MustCompile(`\|\s*tee\b`)
	npm := 0
	for _, w := range workflows(t) {
		for _, j := range w.jobs {
			for _, step := range j.steps {
				name := w.name + " " + j.name + ": " + strings.TrimSpace(strings.SplitN(step, "\n", 2)[0])
				// A pipe into tee hides the test's exit status without pipefail
				// (Actions' default bash has none; shell: bash adds it).
				if pipe.MatchString(step) && !strings.Contains(step, "shell: bash") && !strings.Contains(step, "set -o pipefail") {
					t.Errorf("%s pipes into tee without pipefail", name)
				}
				// A job without its slot fails instead of sharing slot 0.
				if strings.Contains(step, "BACKSIGHT_CI_SLOT:-") {
					t.Errorf("%s defaults BACKSIGHT_CI_SLOT", name)
				}
				// Claude Code's install scripts run with a HOME of their own,
				// not the runner's.
				for _, line := range strings.Split(step, "\n") {
					if strings.Contains(line, "npm install") {
						npm++
						if !regexp.MustCompile(`\bHOME="?\$RUNNER_TEMP/`).MatchString(line) {
							t.Errorf("%s: npm install without an isolated HOME: %s", name, strings.TrimSpace(line))
						}
					}
				}
			}
		}
	}
	if npm == 0 {
		t.Error("no npm install found: the check proves nothing")
	}
}

// The public repository's CI (decision 0068, INDEX rows "0068 runners"
// and "0068 flip timing and runners"): GitHub-hosted runners, the runner
// slot set by the workflow itself, actions pinned to commits.
func TestWorkflowsRunOnHostedRunners(t *testing.T) {
	runsOn := regexp.MustCompile(`(?m)^    runs-on:\s*(.*?)\s*$`)
	slot := regexp.MustCompile(`(?m)^  BACKSIGHT_CI_SLOT: "[1-3]"\s*$`)
	uses := regexp.MustCompile(`(?m)^\s+(?:- )?uses:\s*(.*?)\s*$`)
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\d+\.\d+\.\d+$`)
	run := regexp.MustCompile(`(?m)^      - run:|^        run:`)
	keep := regexp.MustCompile(`(?m)^          persist-credentials: false\s*$`)
	node := regexp.MustCompile(`(?m)^          node-version: "?\d+\.\d+\.\d+"?\s*$`)
	jobs, actions := 0, 0
	for _, w := range workflows(t) {
		runs := false
		for _, j := range w.jobs {
			jobs++
			var on []string
			for _, m := range runsOn.FindAllStringSubmatch(j.raw, -1) {
				on = append(on, m[1])
			}
			if len(on) != 1 || on[0] != "ubuntu-latest" {
				t.Errorf("%s job %s: runs-on %q, want ubuntu-latest only", w.name, j.name, on)
			}
			for _, u := range uses.FindAllStringSubmatch(j.raw, -1) {
				actions++
				if !pinned.MatchString(u[1]) {
					t.Errorf("%s job %s: %q is not pinned to a commit SHA with its version tag", w.name, j.name, u[1])
				}
			}
			for _, step := range j.steps {
				runs = runs || run.MatchString(step)
				// Nothing pushes: the token is not left in .git/config for
				// the steps that follow.
				if strings.Contains(step, "uses: actions/checkout@") && !keep.MatchString(step) {
					t.Errorf("%s job %s: checkout without persist-credentials: false", w.name, j.name)
				}
			}
			// --allow-scripts needs npm 11 or newer (npm 10 rejects it):
			// Node is pinned to an exact version, not the runner's.
			if strings.Contains(j.raw, "npm install") && (!strings.Contains(j.raw, "uses: actions/setup-node@") || !node.MatchString(j.raw)) {
				t.Errorf("%s job %s: npm install without setup-node pinned to an exact Node version", w.name, j.name)
			}
		}
		// A hosted runner has no .env: the workflow sets the slot, once
		// for all its jobs (an unset slot fails on CI, scripts/ports.sh).
		if runs && !slot.MatchString(w.env) {
			t.Errorf("%s runs commands without a workflow-level env BACKSIGHT_CI_SLOT \"1\" to \"3\" (env block: %q)", w.name, w.env)
		}
	}
	if jobs < 7 || actions == 0 {
		t.Errorf("%d jobs and %d actions found, want at least the 7 jobs of ci, gitleaks and release: the checks prove nothing", jobs, actions)
	}
}

// A Docker stack job on a hosted runner first frees the disk the runner's
// preinstalled toolchains take: images, the model cache tar and the data
// fill the rest, and setup's doctor wants 6 GB free. A restored model
// cache tar is dropped once seeded (it is never saved again).
func TestStackJobsFreeTheDiskFirst(t *testing.T) {
	stack := regexp.MustCompile(`scripts/(sandbox|fresh-install)\.sh`)
	jobs := 0
	for _, w := range workflows(t) {
		for _, j := range w.jobs {
			if !stack.MatchString(j.raw) {
				continue
			}
			jobs++
			first := j.steps[0]
			for _, s := range []string{"/usr/share/dotnet", "/usr/local/lib/android", "/opt/ghc", "/opt/hostedtoolcache/CodeQL"} {
				if !strings.Contains(first, "sudo rm -rf") || !strings.Contains(first, s) {
					t.Errorf("%s job %s: its first step does not remove %s", w.name, j.name, s)
				}
			}
			if strings.Count(first, "df -h /") < 2 {
				t.Errorf("%s job %s: its first step does not print df -h / before and after", w.name, j.name)
			}
			for _, step := range j.steps {
				if stack.MatchString(step) && strings.Contains(step, "BACKSIGHT_HF_CACHE=") && !strings.Contains(step, "BACKSIGHT_HF_CACHE_DROP=1") {
					t.Errorf("%s job %s: the model cache tar is kept on disk once seeded (BACKSIGHT_HF_CACHE_DROP=1)", w.name, j.name)
				}
			}
		}
	}
	if jobs < 2 {
		t.Errorf("%d stack jobs found, want sandbox and fresh-install: the check proves nothing", jobs)
	}
}

// A release is published only once its uploaded draft verifies, and the
// signing key is an environment secret (INDEX row "0068 flip timing and
// runners": published as a draft, verified, then made public).
func TestReleasePublishesAVerifiedDraft(t *testing.T) {
	gr, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^release:\n(?:  .*\n)*  draft: true\n`).Match(gr) {
		t.Error(".goreleaser.yaml does not publish a draft (release.draft: true)")
	}
	steps := []struct{ what, re string }{
		{"tag guard", `merge-base --is-ancestor "\$GITHUB_SHA" origin/main`},
		{"goreleaser", `uses: goreleaser/goreleaser-action@`},
		{"verify-dist", `make verify-dist`},
		{"draft check", `gh release download "\$GITHUB_REF_NAME"`},
		{"publish", `gh release edit "\$GITHUB_REF_NAME" .*--draft=false`},
	}
	signers := 0
	for _, w := range workflows(t) {
		for _, j := range w.jobs {
			if strings.Contains(j.raw, "secrets.MINISIGN_SECRET_KEY") {
				signers++
				if w.name != "release.yml" || !regexp.MustCompile(`(?m)^    environment: release\s*$`).MatchString(j.raw) {
					t.Errorf("%s job %s reads MINISIGN_SECRET_KEY outside the release environment", w.name, j.name)
				}
			}
			if w.name != "release.yml" {
				continue
			}
			last := 0
			for _, s := range steps {
				at := 0
				for i, step := range j.steps {
					if regexp.MustCompile(s.re).MatchString(step) {
						at = i + 1
					}
				}
				switch {
				case at == 0:
					t.Errorf("release.yml job %s has no %s step", j.name, s.what)
				case at <= last:
					t.Errorf("release.yml job %s: the %s step (%d) comes before the step it must follow (%d)", j.name, s.what, at, last)
				}
				last = max(last, at)
			}
		}
	}
	if signers == 0 {
		t.Error("no job reads MINISIGN_SECRET_KEY: the checks prove nothing")
	}
}

// setupGo is every actions/setup-go step of the workflows, by job.
func setupGo(t *testing.T) (steps []struct{ where, step string }) {
	t.Helper()
	for _, w := range workflows(t) {
		for _, j := range w.jobs {
			for _, s := range j.steps {
				if strings.Contains(s, "uses: actions/setup-go@") {
					steps = append(steps, struct{ where, step string }{w.name + " job " + j.name, s})
				}
			}
		}
	}
	return steps
}

// Every job builds with the patched toolchain go.mod names, never the
// bare go line's .0 release (security review M2: v0.1.0-m5 shipped with
// go1.26.0 and its known stdlib vulnerabilities). setup-go reads only the
// go line of go-version-file, so each step names the version itself.
func TestWorkflowsBuildWithThePinnedToolchain(t *testing.T) {
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^toolchain go(\d+\.\d+\.\d+)\s*$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no toolchain goX.Y.Z line: builds take the go line's release, unpatched")
	}
	want := regexp.MustCompile(`(?m)^          go-version: "?` + regexp.QuoteMeta(string(m[1])) + `"?(\s+#.*)?$`)
	steps := setupGo(t)
	for _, s := range steps {
		if !want.MatchString(s.step) || strings.Contains(s.step, "go-version-file") {
			t.Errorf("%s: setup-go does not install go.mod's toolchain %s exactly:\n%s", s.where, m[1], s.step)
		}
	}
	if len(steps) < 6 {
		t.Errorf("%d setup-go steps found: the check proves little", len(steps))
	}
}

// A job that signs (goreleaser, the release key) restores no cache: the
// Go build cache is shared with every main-branch job, some of which run
// third-party tools, and GOCACHE objects are trusted on a hit (security
// review M4, the "cacheract" pattern).
func TestSigningJobsRestoreNoCache(t *testing.T) {
	signers := 0
	for _, w := range workflows(t) {
		for _, j := range w.jobs {
			if !strings.Contains(j.raw, "goreleaser/goreleaser-action@") && !strings.Contains(j.raw, "MINISIGN_SECRET_KEY") {
				continue
			}
			signers++
			for _, s := range j.steps {
				if strings.Contains(s, "uses: actions/setup-go@") && !regexp.MustCompile(`(?m)^          cache: false\s*$`).MatchString(s) {
					t.Errorf("%s job %s signs but setup-go restores its cache (cache: false missing)", w.name, j.name)
				}
				if strings.Contains(s, "uses: actions/cache") {
					t.Errorf("%s job %s signs but restores a cache: %s", w.name, j.name, strings.TrimSpace(strings.SplitN(s, "\n", 2)[0]))
				}
			}
		}
	}
	if signers < 2 {
		t.Errorf("%d signing jobs found, want release and snapshot: the check proves nothing", signers)
	}
}

// govulncheck, at a pinned version, fails CI on a vulnerability our code
// reaches, on every change and weekly, as new advisories land on an
// unchanged tree (security review M2).
func TestGovulncheckGuardsTheCode(t *testing.T) {
	run := regexp.MustCompile(`go run golang\.org/x/vuln/cmd/govulncheck@v\d+\.\d+\.\d+ \./\.\.\.`)
	found := 0
	for _, w := range workflows(t) {
		raw, err := os.ReadFile(filepath.Join("../.github/workflows", w.name))
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range w.jobs {
			if !run.MatchString(j.raw) {
				continue
			}
			found++
			for _, on := range []string{`(?m)^  pull_request:`, `(?m)^  push:\n    branches: \[main\]`, `(?m)^  schedule:\n    - cron: "[^"]+"`} {
				if !regexp.MustCompile(on).Match(raw) {
					t.Errorf("%s runs govulncheck but not on %s", w.name, on)
				}
			}
		}
	}
	if found == 0 {
		t.Error("no job runs govulncheck@<version> ./...")
	}
}

// Dependabot keeps the modules and the actions' SHA pins current, weekly,
// one grouped pull request per ecosystem (security review M2).
func TestDependabotTracksModulesAndActions(t *testing.T) {
	raw, err := os.ReadFile("../.github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	updates := blocks(string(raw), regexp.MustCompile(`(?m)^  - package-ecosystem:`))
	seen := map[string]bool{}
	for _, u := range updates {
		eco := strings.Trim(strings.TrimSpace(strings.SplitN(strings.SplitN(u, ":", 2)[1], "\n", 2)[0]), `"'`)
		seen[eco] = true
		for _, want := range []string{`(?m)^      interval: "?weekly"?\s*$`, `(?m)^    groups:\s*$`, `(?m)^    directory: "?/"?\s*$`} {
			if !regexp.MustCompile(want).MatchString(u) {
				t.Errorf("dependabot %s: no %s", eco, want)
			}
		}
	}
	for _, eco := range []string{"gomod", "github-actions"} {
		if !seen[eco] {
			t.Errorf("dependabot.yml does not track %s (found %v)", eco, seen)
		}
	}
}

// releaseNotesStep is the release job's step that refuses a tag without
// its hand-written notes, and the goreleaser step that publishes them.
func releaseNotesStep(t *testing.T) (check, goreleaser string, order []int) {
	t.Helper()
	for _, w := range workflows(t) {
		if w.name != "release.yml" {
			continue
		}
		for _, j := range w.jobs {
			for i, step := range j.steps {
				switch {
				case strings.Contains(step, `notes="docs/releases/$GITHUB_REF_NAME.md"`):
					check = step
					order = append(order, i)
				case strings.Contains(step, "uses: goreleaser/goreleaser-action@"):
					goreleaser = step
					order = append(order, i)
				}
			}
		}
	}
	if check == "" || goreleaser == "" {
		t.Fatalf("release.yml: notes step found %v, goreleaser step found %v", check != "", goreleaser != "")
	}
	return check, goreleaser, order
}

// A release publishes its hand-written notes (docs/releases/<tag>.md), not
// goreleaser's list of commit subjects, and a tag without them, or without
// its row in docs/compatibility.md, stops before anything is built.
func TestReleasePublishesHandWrittenNotes(t *testing.T) {
	check, goreleaser, order := releaseNotesStep(t)
	if len(order) != 2 || order[0] > order[1] {
		t.Errorf("release.yml: the notes check must come before goreleaser (steps %v)", order)
	}
	if !regexp.MustCompile(`(?m)^\s+args: release --clean --release-notes docs/releases/\$\{\{ github\.ref_name \}\}\.md\s*$`).MatchString(goreleaser) {
		t.Errorf("release.yml: goreleaser does not publish docs/releases/<tag>.md:\n%s", goreleaser)
	}
	gr, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// changelog.disable would skip --release-notes too (goreleaser 2.14).
	if regexp.MustCompile(`(?m)^changelog:\n(?:  .*\n)*  disable:`).Match(gr) {
		t.Error(".goreleaser.yaml disables the changelog: that drops --release-notes, and the release would have no notes")
	}

	_, body, ok := strings.Cut(check, "run: |\n")
	if !ok {
		t.Fatalf("the notes step has no run block:\n%s", check)
	}
	script := regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body, "")
	const row = "| Release | x |\n|---|---|\n| v9.8.7 | 1.1.0 |\n"
	cases := []struct {
		name, notes, compat string
		ok                  bool
		says                string
	}{
		{"notes and row", "What it is.\n", row, true, ""},
		{"no notes", "", row, false, "docs/releases/v9.8.7.md is missing or empty"},
		{"empty notes", "-", row, false, "docs/releases/v9.8.7.md is missing or empty"},
		{"no row", "What it is.\n", "| v9.8.70 | 1.1.0 |\n", false, "docs/compatibility.md has no row for v9.8.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "docs", "releases"), 0o755); err != nil {
				t.Fatal(err)
			}
			switch c.notes {
			case "":
			case "-":
				_ = os.WriteFile(filepath.Join(dir, "docs", "releases", "v9.8.7.md"), nil, 0o644)
			default:
				_ = os.WriteFile(filepath.Join(dir, "docs", "releases", "v9.8.7.md"), []byte(c.notes), 0o644)
			}
			_ = os.WriteFile(filepath.Join(dir, "docs", "compatibility.md"), []byte(c.compat), 0o644)
			cmd := exec.Command("bash", "-eo", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GITHUB_REF_NAME=v9.8.7")
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.ok || !strings.Contains(string(out), c.says) {
				t.Errorf("ok %v, want %v; output %q, want it to say %q", err == nil, c.ok, out, c.says)
			}
		})
	}
}
