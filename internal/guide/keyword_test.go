package guide

import (
	"reflect"
	"testing"
)

func refs(hits []Hit) []string {
	out := []string{}
	for _, h := range hits {
		out = append(out, h.Ref())
	}
	return out
}

func TestKeywordRanksTheMatchingSectionFirst(t *testing.T) {
	c := load(t)
	for q, want := range map[string]string{
		"how do I restore a backup":  "backup#restore-a-backup",
		"remove the server volumes":  "install#uninstall",
		"Docker requirements":        "install#requirements",
		"encrypted archive age":      "backup#make-a-backup",
		"RESTORING backups":          "backup#restore-a-backup", // case and plural folded
		"désinstaller --delete-data": "install#uninstall",       // a flag still matches
	} {
		hits := c.Keyword(q, 3)
		if len(hits) == 0 || hits[0].Ref() != want {
			t.Errorf("%q: got %v, want %s first", q, refs(hits), want)
		}
		if len(hits) > 3 {
			t.Errorf("%q: limit 3, got %d", q, len(hits))
		}
	}
}

func TestKeywordIsDeterministicAndDropsNonMatches(t *testing.T) {
	c := load(t)
	a, b := c.Keyword("backsight", 10), c.Keyword("backsight", 10)
	if !reflect.DeepEqual(refs(a), refs(b)) || len(a) == 0 {
		t.Errorf("two runs differ or found nothing: %v %v", refs(a), refs(b))
	}
	for i := 1; i < len(a); i++ {
		if a[i].Score > a[i-1].Score {
			t.Errorf("not sorted by score: %v", a)
		}
	}
	if hits := c.Keyword("kubernetes helm", 5); len(hits) != 0 {
		t.Errorf("no word matches: got %v", refs(hits))
	}
	if hits := c.Keyword("the and of", 5); len(hits) != 0 {
		t.Errorf("stop words alone match nothing: got %v", refs(hits))
	}
	if hits := c.Keyword("restore", 5); hits[0].Excerpt == "" || hits[0].Excerpt[0] == '#' {
		t.Errorf("excerpt %q", hits[0].Excerpt)
	}
}
