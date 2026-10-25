package guide

import (
	"strings"
	"testing"
	"testing/fstest"
)

// fixture is a small docs tree: two topics, a code fence holding a "##"
// line, a repeated heading and a preamble.
func fixture() fstest.MapFS {
	return fstest.MapFS{
		"backup.md": {Data: []byte("# Backups and restore\n\nbacksight keeps encrypted backups of the server.\n\n" +
			"## Make a backup\n\nRun `backsight backup create`. The archive is encrypted with age.\n\n" +
			"```sh\n## not a heading\nbacksight backup list\n```\n\n" +
			"## Restore a backup\n\nRun `backsight restore <file>`: the server stops, the database is replaced, every client reconciles.\n\n" +
			"### Rollback\n\nA failed restore rolls back to the safety archive.\n")},
		"install.md": {Data: []byte("# Install\n\n## Requirements\n\nLinux, Docker and Claude Code.\n\n" +
			"## Uninstall\n\nRun `backsight uninstall`; `--delete-data` also removes the server volumes.\n\n" +
			"## Uninstall\n\nA second heading of the same name.\n")},
		"notes.txt": {Data: []byte("not a topic")},
	}
}

func load(t *testing.T) Corpus {
	t.Helper()
	c, err := Load(fixture())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadSplitsOnSecondLevelHeadings(t *testing.T) {
	c := load(t)
	if len(c.Topics) != 2 || c.Topics[0].Name != "backup" || c.Topics[0].Title != "Backups and restore" || c.Topics[1].Name != "install" {
		t.Fatalf("topics %+v", c.Topics)
	}
	var ids []string
	for _, s := range c.Sections() {
		ids = append(ids, s.ID())
	}
	want := "docs:backup, docs:backup#make-a-backup, docs:backup#restore-a-backup, docs:install#requirements, docs:install#uninstall, docs:install#uninstall-1"
	if got := strings.Join(ids, ", "); got != want {
		t.Errorf("sections\n got %s\nwant %s", got, want)
	}
	make, _ := c.Section("backup#make-a-backup")
	if !strings.Contains(make.Text, "## not a heading") || !strings.HasPrefix(make.Text, "# Backups and restore\n\n## Make a backup\n") {
		t.Errorf("a fenced ## line split the section, or the title is missing:\n%s", make.Text)
	}
	restore, _ := c.Section("backup#restore-a-backup")
	if !strings.Contains(restore.Text, "### Rollback") || restore.Heading != "Restore a backup" {
		t.Errorf("### belongs to its ## section: %+v", restore)
	}
	intro, ok := c.Section("backup")
	if !ok || intro.Heading != "Backups and restore" || !strings.Contains(intro.Text, "encrypted backups") || strings.Contains(intro.Text, "## Make") {
		t.Errorf("preamble %+v", intro)
	}
	if _, ok := c.Section("install"); ok {
		t.Error("a topic with nothing before its first ## has no preamble section")
	}
}

func TestSectionHashFollowsItsTextOnly(t *testing.T) {
	a := load(t)
	fs := fixture()
	fs["install.md"] = fstestFile{"# Install\n\n## Requirements\n\nLinux, Docker, Claude Code and an LLM key.\n\n## Uninstall\n\nRun `backsight uninstall`; `--delete-data` also removes the server volumes.\n\n## Uninstall\n\nA second heading of the same name.\n"}.mapFile()
	b, err := Load(fs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash == b.Hash || a.Tag() == b.Tag() || !strings.HasPrefix(a.Tag(), "docs-version:") {
		t.Errorf("docs hash must change with any doc: %s %s", a.Tag(), b.Tag())
	}
	for _, ref := range []string{"install#uninstall", "backup#make-a-backup", "backup"} {
		x, _ := a.Section(ref)
		y, _ := b.Section(ref)
		if x.Hash == "" || x.Hash != y.Hash {
			t.Errorf("%s: unchanged section changed hash", ref)
		}
	}
	x, _ := a.Section("install#requirements")
	y, _ := b.Section("install#requirements")
	if x.Hash == y.Hash {
		t.Error("edited section kept its hash")
	}
}

func TestLoadRefusesAnEmptyTree(t *testing.T) {
	if _, err := Load(fstest.MapFS{"a.txt": {Data: []byte("x")}}); err == nil {
		t.Error("no .md file must be an error")
	}
}

func TestSlugsFollowGitHubAnchors(t *testing.T) {
	for in, want := range map[string]string{
		"1. Install the binary":     "1-install-the-binary",
		"When something goes wrong": "when-something-goes-wrong",
		"`guide search` and --json": "guide-search-and---json",
		"Sauvegardes : où, quand ?": "sauvegardes--où-quand-",
		"Snake_case stays":          "snake_case-stays",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShippedDocsLoad(t *testing.T) {
	c, err := Embedded()
	if err != nil || len(c.Topics) == 0 || c.Hash == "" {
		t.Fatalf("embedded docs: %v %+v", err, c.Topics)
	}
	seen := map[string]bool{}
	for _, s := range c.Sections() {
		if seen[s.ID()] {
			t.Errorf("duplicate section id %s", s.ID())
		}
		seen[s.ID()] = true
	}
}

type fstestFile struct{ data string }

func (f fstestFile) mapFile() *fstest.MapFile { return &fstest.MapFile{Data: []byte(f.data)} }
