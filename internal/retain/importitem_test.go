package retain

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func importFile() ImportFile {
	return ImportFile{Source: "abc", Rel: "a/b.md", DocumentID: "import:abc/a/b.md", Content: "hello",
		SHA256: "h1", Project: "P", MachineID: "m1", Version: "v9", Date: time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)}
}

func TestImportItemFields(t *testing.T) {
	item, op := ImportItem("bank", importFile())
	if item.Strategy != "import" || item.Context != "file imported from a/b.md" || item.DocumentID != "import:abc/a/b.md" || item.Content != "hello" {
		t.Errorf("item = %+v", item)
	}
	if !reflect.DeepEqual(item.Tags, []string{"project:P", "source:import"}) {
		t.Errorf("tags = %v", item.Tags)
	}
	if !reflect.DeepEqual(item.ObservationScopes, [][]string{{"project:P"}}) {
		t.Errorf("scopes = %v", item.ObservationScopes)
	}
	if item.Timestamp == nil || !item.Timestamp.Equal(importFile().Date) {
		t.Errorf("timestamp = %v", item.Timestamp)
	}
	var keys []string
	for k := range item.Metadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !reflect.DeepEqual(keys, []string{"binary_version", "machine_id", "path", "sha256", "source"}) {
		t.Errorf("metadata keys = %v", keys)
	}
	if item.Metadata["path"] != "a/b.md" || item.Metadata["sha256"] != "h1" || item.Metadata["source"] != "abc" {
		t.Errorf("metadata = %v", item.Metadata)
	}
	if item.UpdateMode != "" || op == "" {
		t.Errorf("update mode %q, operation %q", item.UpdateMode, op)
	}
}

func TestImportOperationIDCollapsesRepeats(t *testing.T) {
	f := importFile()
	_, a := ImportItem("bank", f)
	_, b := ImportItem("bank", f)
	f.SHA256 = "h2"
	_, c := ImportItem("bank", f)
	_, d := ImportItem("other", importFile())
	if a != b {
		t.Error("same file, two operation ids")
	}
	if a == c || a == d {
		t.Error("changed content or bank kept the operation id")
	}
}

func TestImportOperationIDOfAMove(t *testing.T) {
	f := importFile()
	_, sent := ImportItem("bank", f)
	// The same content sent again to another project is a new operation:
	// a collapse would leave it in the old one.
	f.Project, f.MovedFrom = "renamed", f.Project
	item, moved := ImportItem("bank", f)
	_, again := ImportItem("bank", f)
	back := f
	back.Project, back.MovedFrom = f.MovedFrom, "renamed"
	_, returned := ImportItem("bank", back)
	if moved == sent || returned == sent || returned == moved {
		t.Errorf("operations %s, %s, %s: a move must not collapse with an earlier send", sent, moved, returned)
	}
	if moved != again {
		t.Error("the same move, two operation ids")
	}
	if !slices.Contains(item.Tags, "project:renamed") || item.ObservationScopes[0][0] != "project:renamed" {
		t.Errorf("tags %v scopes %v", item.Tags, item.ObservationScopes)
	}
}
