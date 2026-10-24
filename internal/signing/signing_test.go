package signing

import (
	"os"
	"strings"
	"testing"
)

// The embedded keys must be the ones published at the repository root.
func TestEmbeddedKeysMatchRepository(t *testing.T) {
	for file, got := range map[string]string{"../../minisign.pub": Current, "../../minisign.next.pub": Next} {
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from the embedded key", file)
		}
	}
	if Current == Next {
		t.Error("the next key must differ from the current one")
	}
}

func TestKeyIDs(t *testing.T) {
	for _, k := range []string{Current, Next} {
		if !strings.HasPrefix(k, "untrusted comment: minisign public key ") || len(strings.Split(strings.TrimSpace(k), "\n")) != 2 {
			t.Errorf("not a minisign public key file: %q", k)
		}
	}
}
