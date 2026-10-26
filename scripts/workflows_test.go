package scripts

import (
	"os"
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
