package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every `backsight <command> [subcommand] [--flag]` the user documentation
// names (README.md, CONTRIBUTING.md, SECURITY.md, docs/*.md) must exist in
// the command tree (backsight issue 240): a renamed command or flag that
// forgets the docs fails here. Only code is read (inline `code` spans and
// fenced blocks), since prose says "backsight" as a name.

// docsPending are commands the docs already name that another block-4
// issue builds. Each is accepted only while it is missing; once built, it
// is checked like any other and the test asks for its entry to be removed.
var docsPending = []string{}

// docFiles lists the documents checked, relative to the repository root.
func docFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"README.md", "CONTRIBUTING.md", "SECURITY.md"}
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs/*.md: %v", err)
	}
	for _, d := range docs {
		files = append(files, filepath.Join("docs", filepath.Base(d)))
	}
	return files
}

var (
	fence      = regexp.MustCompile("^\\s*(```|~~~)")
	inlineCode = regexp.MustCompile("`([^`\n]+)`")
)

// codeTexts returns the code in a Markdown document: each fenced block
// line and each inline code span, with its line number.
func codeTexts(md string) []docCode {
	var out []docCode
	in := false
	for i, line := range strings.Split(md, "\n") {
		if fence.MatchString(line) {
			in = !in
			continue
		}
		if in {
			out = append(out, docCode{i + 1, line})
			continue
		}
		for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
			out = append(out, docCode{i + 1, m[1]})
		}
	}
	return out
}

type docCode struct {
	line int
	text string
}

// mentions returns each "backsight …" invocation in a code text, as its
// words after "backsight", cut at a shell separator or a comment.
func mentions(code string) [][]string {
	var out [][]string
	words := strings.Fields(code)
	for i := 0; i < len(words); i++ {
		if strings.TrimLeft(words[i], "!($") != "backsight" {
			continue
		}
		var args []string
		for i++; i < len(words); i++ {
			w := words[i]
			if w == "|" || w == "||" || w == "&&" || w == ";" || strings.HasPrefix(w, "#") ||
				strings.HasPrefix(w, ">") || strings.HasPrefix(w, "2>") {
				break
			}
			args = append(args, w)
		}
		i-- // the separator may start another mention
		if len(args) > 0 {
			out = append(out, args)
		}
	}
	return out
}

// trimWord drops the brackets and punctuation around a documented word:
// "[--plan]" is "--plan", "--days=N" is "--days", "flag)." is "flag".
func trimWord(w string) string {
	w = strings.Trim(w, "[](){},.:;\"'`…")
	if name, _, ok := strings.Cut(w, "="); ok && strings.HasPrefix(name, "-") {
		w = name
	}
	return w
}

var commandWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// findSub returns cmd's subcommand called name (hidden ones included).
func findSub(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name || slices.Contains(c.Aliases, name) {
			return c
		}
	}
	return nil
}

// checkMention resolves args against the tree under root and returns what
// does not exist: an unknown command or subcommand, or a flag the resolved
// command does not take. pending names commands accepted while missing.
func checkMention(root *cobra.Command, args []string, pending []string) error {
	cmd := root
	path := ""
	i := 0
	resolved := true // false once a placeholder or alternatives stand for the command
	for ; i < len(args); i++ {
		w := trimWord(args[i])
		if strings.HasPrefix(w, "-") || !cmd.HasSubCommands() {
			break
		}
		if strings.HasPrefix(w, "<") { // `backsight <command> --help`
			resolved = false
			i++
			break
		}
		if strings.Contains(w, "|") { // `backsight backup create|verify|list`
			for _, alt := range strings.Split(w, "|") {
				if alt != "" && findSub(cmd, alt) == nil && !slices.Contains(pending, strings.TrimSpace(path+" "+alt)) {
					return fmt.Errorf("%q has no subcommand %q", strings.TrimSpace("backsight "+path), alt)
				}
			}
			resolved = false
			i++
			break
		}
		sub := findSub(cmd, w)
		next := strings.TrimSpace(path + " " + w)
		if sub == nil {
			if slices.Contains(pending, next) {
				return nil // not built yet: nothing more to check
			}
			if !commandWord.MatchString(w) || (cmd.HasParent() && cmd.ValidateArgs([]string{w}) == nil) {
				break // an argument of a command that takes one
			}
			return fmt.Errorf("%q has no subcommand %q", strings.TrimSpace("backsight "+path), w)
		}
		cmd, path = sub, next
	}
	for ; i < len(args); i++ {
		w := trimWord(args[i])
		if !strings.HasPrefix(w, "--") {
			continue // a value, an argument or a short flag
		}
		for _, f := range strings.Split(w, "|") { // [--stop|--delete-data]
			f = strings.TrimPrefix(trimWord(f), "--")
			if f == "" {
				continue
			}
			if resolved && cmd.Flag(f) == nil {
				return fmt.Errorf("%q has no flag --%s", "backsight "+path, f)
			}
			if !resolved && root.PersistentFlags().Lookup(f) == nil && f != "help" {
				return fmt.Errorf("--%s is not a global flag (the command is a placeholder)", f)
			}
		}
	}
	return nil
}

// builtPending returns the pending entries the tree now has.
func builtPending(root *cobra.Command, pending []string) []string {
	var out []string
	for _, p := range pending {
		c, rest, err := root.Find(strings.Fields(p))
		if err == nil && len(rest) == 0 && c.CommandPath() == "backsight "+p {
			out = append(out, p)
		}
	}
	return out
}

func docsRoot() *cobra.Command {
	return newRoot(&env{stdout: io.Discard, stderr: io.Discard})
}

func TestDocsNameOnlyExistingCommandsAndFlags(t *testing.T) {
	root := docsRoot()
	for _, p := range builtPending(root, docsPending) {
		t.Errorf("backsight %s is built now: remove it from docsPending", p)
	}
	perFile := map[string]int{}
	for _, f := range docFiles(t) {
		raw, err := os.ReadFile(filepath.Join("../..", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range codeTexts(string(raw)) {
			for _, m := range mentions(c.text) {
				perFile[f]++
				if err := checkMention(root, m, docsPending); err != nil {
					t.Errorf("%s:%d: `backsight %s`: %v", f, c.line, strings.Join(m, " "), err)
				}
			}
		}
	}
	// The scan must find what it checks: the landing page, the contributor
	// guide and the command reference all name commands.
	for _, f := range []string{"README.md", "CONTRIBUTING.md", "docs/commands.md", "docs/install.md", "docs/troubleshooting.md"} {
		if perFile[f] < 3 {
			t.Errorf("%s: only %d backsight command(s) found; the scan reads nothing?", f, perFile[f])
		}
	}
}

func TestCheckMention(t *testing.T) {
	root := docsRoot()
	ok := []string{
		"setup",
		"setup --plan --json --answer provider=deepseek",
		"setup --undo [--plan]",
		"backup create|verify|list",
		"backup verify <file> [--identity F]",
		"secret set llm_api_key --from-env=KEY",
		"secret set llm_api_key --from-file=key.txt",
		"search decisions pitfalls --limit 3",
		"stats 30",
		"uninstall [--stop|--delete-data] [--purge-local]",
		"jobs refresh --plan", // hidden commands count
		"<command> --help",
		"<command> --json",
		"client config restore",
		"guide search why", // positional words after a built command
		"guide",
	}
	for _, m := range ok {
		if err := checkMention(root, strings.Fields(m), docsPending); err != nil {
			t.Errorf("`backsight %s`: %v", m, err)
		}
	}
	bad := map[string]string{
		"setpu":                      `"backsight" has no subcommand "setpu"`,
		"setup --plann":              `"backsight setup" has no flag --plann`,
		"backup creat":               `"backsight backup" has no subcommand "creat"`,
		"backup create|verfy":        `"backsight backup" has no subcommand "verfy"`,
		"uninstall [--stop|--delet]": `"backsight uninstall" has no flag --delet`,
		"server upgrade --force":     `"backsight server upgrade" has no flag --force`,
		"client config undo":         `"backsight client config" has no subcommand "undo"`,
		"restore f.age --no-backup":  `"backsight restore" has no flag --no-backup`,
		"<command> --plan":           "--plan is not a global flag (the command is a placeholder)",
		"guidex search":              `"backsight" has no subcommand "guidex"`,
	}
	for m, want := range bad {
		err := checkMention(root, strings.Fields(m), docsPending)
		if err == nil || err.Error() != want {
			t.Errorf("`backsight %s`: got %v, want %s", m, err, want)
		}
	}
}

// A pending command is accepted only while it is missing, and only that
// command: a neighbour's typo still fails, and so does the same mention
// once the entry is gone. A pending entry the tree has is reported.
func TestCheckMentionPendingCommands(t *testing.T) {
	root := docsRoot()
	pending := []string{"backup prune"}
	for _, m := range []string{"backup prune", "backup prune --older-than 30d", "backup create|prune"} {
		if err := checkMention(root, strings.Fields(m), pending); err != nil {
			t.Errorf("pending `backsight %s`: %v", m, err)
		}
		if err := checkMention(root, strings.Fields(m), nil); err == nil || err.Error() != `"backsight backup" has no subcommand "prune"` {
			t.Errorf("`backsight %s` with nothing pending: got %v", m, err)
		}
	}
	for m, want := range map[string]string{
		"backup prnue":        `"backsight backup" has no subcommand "prnue"`,
		"backup create|prnue": `"backsight backup" has no subcommand "prnue"`,
		"backup prune2":       `"backsight backup" has no subcommand "prune2"`,
	} {
		if err := checkMention(root, strings.Fields(m), pending); err == nil || err.Error() != want {
			t.Errorf("`backsight %s` with backup prune pending: got %v, want %s", m, err, want)
		}
	}
	if got := builtPending(root, []string{"backup prune", "backup create", "guide index"}); !slices.Equal(got, []string{"backup create", "guide index"}) {
		t.Errorf("builtPending = %q, want the two built commands", got)
	}
}

func TestMentions(t *testing.T) {
	cases := map[string][][]string{
		"backsight doctor --json":                             {{"doctor", "--json"}},
		"! backsight secret set llm_api_key":                  {{"secret", "set", "llm_api_key"}},
		"backsight backup list && backsight restore f --plan": {{"backup", "list"}, {"restore", "f", "--plan"}},
		"backsight update check  # daily":                     {{"update", "check"}},
		"curl -fsSL https://x/install.sh | sh":                nil,
		"~/.local/bin/backsight":                              nil,
		"backsight":                                           nil,
		"BACKSIGHT_HOME=/tmp/b backsight paths":               {{"paths"}},
	}
	for in, want := range cases {
		if got := mentions(in); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("mentions(%q) = %q, want %q", in, got, want)
		}
	}
	md := "text `backsight a` and `x`\n```sh\nbacksight b\n```\n`backsight c`"
	var got []string
	for _, c := range codeTexts(md) {
		got = append(got, fmt.Sprintf("%d:%s", c.line, c.text))
	}
	if want := []string{"1:backsight a", "1:x", "3:backsight b", "5:backsight c"}; !slices.Equal(got, want) {
		t.Errorf("codeTexts = %q, want %q", got, want)
	}
}

// readmeHindsight returns the Hindsight version the README's badge states.
var hindsightBadge = regexp.MustCompile(`\[!\[Hindsight ([^\]]+)\]\(https://img\.shields\.io/badge/Hindsight-([^-)]+)-[a-z]+\)\]`)

func readmeHindsight(readme string) (string, error) {
	ms := hindsightBadge.FindAllStringSubmatch(readme, -1)
	if len(ms) != 1 {
		return "", fmt.Errorf("want one Hindsight badge, found %d", len(ms))
	}
	if ms[0][1] != ms[0][2] {
		return "", fmt.Errorf("the badge's text says %s, its image %s", ms[0][1], ms[0][2])
	}
	return ms[0][1], nil
}

// The README states the Hindsight version backsight pins (INDEX "0069
// README badges"), as docs/compatibility.md's current row does.
func TestReadmeStatesThePinnedHindsight(t *testing.T) {
	image := mustDocValue(t, compatibilityDoc, "Current", "Hindsight server")
	_, tag, ok := strings.Cut(image, "hindsight:")
	tag, _, _ = strings.Cut(tag, "@")
	if !ok || tag == "" {
		t.Fatalf("no tag in %q", image)
	}
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readmeHindsight(string(raw))
	if err != nil {
		t.Fatalf("README.md: %v", err)
	}
	if got != tag {
		t.Errorf("README.md's badge says Hindsight %s, docs/compatibility.md pins %s", got, tag)
	}
}

func TestReadmeHindsightBadge(t *testing.T) {
	badge := func(text, img string) string {
		return "[![Hindsight " + text + "](https://img.shields.io/badge/Hindsight-" + img + "-blue)](docs/compatibility.md)"
	}
	if v, err := readmeHindsight("x\n" + badge("1.2.0", "1.2.0") + "\n"); err != nil || v != "1.2.0" {
		t.Errorf("one badge: %q, %v", v, err)
	}
	for name, md := range map[string]string{
		"none":       "# backsight\n",
		"two":        badge("1.1.0", "1.1.0") + badge("1.2.0", "1.2.0"),
		"text drift": badge("1.1.0", "1.2.0"),
	} {
		if _, err := readmeHindsight(md); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
