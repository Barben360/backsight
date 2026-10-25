package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowSteps splits each workflow into its steps (the blocks starting
// at a job's "      - "), keyed "<file> step <n>".
func workflowSteps(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	steps := map[string]string{}
	start := regexp.MustCompile(`(?m)^      - `)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		idx := start.FindAllStringIndex(string(raw), -1)
		for i, at := range idx {
			end := len(raw)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			steps[filepath.Base(f)+" step "+strings.TrimSpace(string(raw[at[0]:min(at[0]+60, end)]))] = string(raw[at[0]:end])
		}
	}
	return steps
}

// The CI rules of decision 0067's block review, which a workflow edit
// could silently drop.
func TestWorkflowsKeepTheirSafetyRules(t *testing.T) {
	pipe := regexp.MustCompile(`\|\s*tee\b`)
	npm := 0
	for name, step := range workflowSteps(t) {
		// A pipe into tee hides the test's exit status without pipefail
		// (Actions' default bash has none; shell: bash adds it).
		if pipe.MatchString(step) && !strings.Contains(step, "shell: bash") && !strings.Contains(step, "set -o pipefail") {
			t.Errorf("%s pipes into tee without pipefail", name)
		}
		// A runner without its slot fails instead of sharing slot 0.
		if strings.Contains(step, "BACKSIGHT_CI_SLOT:-") {
			t.Errorf("%s defaults BACKSIGHT_CI_SLOT", name)
		}
		// Claude Code's install scripts run with a HOME of their own, not
		// the runner's (the machine's live install).
		for _, line := range strings.Split(step, "\n") {
			if strings.Contains(line, "npm install") {
				npm++
				if !regexp.MustCompile(`\bHOME="?\$RUNNER_TEMP/`).MatchString(line) {
					t.Errorf("%s: npm install without an isolated HOME: %s", name, strings.TrimSpace(line))
				}
			}
		}
	}
	if npm == 0 {
		t.Error("no npm install found: the check proves nothing")
	}
}
