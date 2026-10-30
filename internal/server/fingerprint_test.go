package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fpDB answers the fingerprint's queries from rows a test sets: each
// table's keys (the text Postgres would give each row's key), the banks,
// and the control documents' retain_params.
type fpDB struct {
	Database
	keys    map[string][]string // table → each row's key text
	noPK    []string            // tables without a primary key
	banks   []string
	control map[string]string // control document id → retain_params text
	queries []string
}

func (f *fpDB) Query(_ context.Context, sql string) (string, error) {
	f.queries = append(f.queries, sql)
	md := func(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
	switch {
	case strings.HasPrefix(sql, "/* backsight:tables */"):
		var out []fpTable
		for _, t := range slices.Sorted(maps.Keys(f.keys)) {
			k := []string{"id"}
			if slices.Contains(f.noPK, t) {
				k = []string{}
			}
			out = append(out, fpTable{Name: t, Keys: k})
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	case strings.HasPrefix(sql, "/* backsight:table "):
		t := strings.Fields(sql)[2]
		ks := slices.Clone(f.keys[t])
		slices.Sort(ks)
		return fmt.Sprintf("%d %s", len(ks), md(strings.Join(ks, "\n"))), nil
	case strings.HasPrefix(sql, "/* backsight:keys "):
		t := strings.Fields(sql)[2]
		ks := slices.Clone(f.keys[t])
		slices.Sort(ks)
		raw, _ := json.Marshal(ks)
		return string(raw), nil
	case strings.HasPrefix(sql, "/* backsight:control */"):
		if !strings.Contains(sql, "'claude-memory-control'") {
			return "", fmt.Errorf("control documents of another bank: %s", sql)
		}
		out := map[string]string{}
		for id, p := range f.control {
			out[id] = md(p)
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	case strings.Contains(sql, "from banks"):
		raw, _ := json.Marshal(f.banks)
		return string(raw), nil
	}
	return "", fmt.Errorf("unexpected query %s", sql)
}

func (f *fpDB) BankIDs(ctx context.Context) ([]string, error) { return bankIDs(ctx, f.Query) }

func newFPDB() *fpDB {
	return &fpDB{
		keys: map[string][]string{
			"documents":    {"(d1)", "(d2)", "(d3)"},
			"memory_units": {"(m1)", "(m2)"},
			"audit":        {"(x,1)", "(y,2)"},
		},
		noPK:    []string{"audit"},
		banks:   []string{"claude-memory", "claude-memory-control"},
		control: map[string]string{"backsight:server-id": `{"metadata": {"id": "s1"}}`, "backsight:epoch": `{"metadata": {"epoch": "2"}}`},
	}
}

func fingerprintOf(t *testing.T, db *fpDB) Fingerprint {
	t.Helper()
	fp, err := TakeFingerprint(context.Background(), db, "claude-memory")
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// The gate sees what row counts miss: a key changed with the counts
// equal, a control document's metadata changed, a bank gone.
func TestFingerprintSeesWhatCountsMiss(t *testing.T) {
	src := newFPDB()
	a := fingerprintOf(t, src)
	if len(a.Tables) != 3 || a.Tables["documents"].Rows != 3 || len(a.Control) != 2 || len(a.Banks) != 2 {
		t.Fatalf("fingerprint %+v", a)
	}
	if d := a.Diff(fingerprintOf(t, newFPDB())); len(d) != 0 {
		t.Fatalf("equal data differs: %q", d)
	}

	for _, c := range []struct {
		name   string
		change func(*fpDB)
		want   string
	}{
		{"a key changed, counts equal", func(f *fpDB) { f.keys["documents"][1] = "(d9)" }, "documents: keys differ"},
		{"a row gone", func(f *fpDB) { f.keys["memory_units"] = f.keys["memory_units"][:1] }, "memory_units: rows 2 → 1"},
		{"a row of a table without a primary key changed", func(f *fpDB) { f.keys["audit"][0] = "(x,3)" }, "audit: keys differ"},
		{"a control document's retain_params changed", func(f *fpDB) { f.control["backsight:epoch"] = `{"metadata": {"epoch": "3"}}` },
			"control document backsight:epoch differs"},
		{"a control document gone", func(f *fpDB) { delete(f.control, "backsight:server-id") }, "control document backsight:server-id missing"},
		{"a bank missing", func(f *fpDB) { f.banks = f.banks[:1] }, "bank claude-memory-control missing"},
		{"a table missing", func(f *fpDB) { delete(f.keys, "audit") }, "table audit missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dst := newFPDB()
			c.change(dst)
			d := a.Diff(fingerprintOf(t, dst))
			if !slices.Equal(d, []string{c.want}) {
				t.Errorf("diff %q, want [%q]", d, c.want)
			}
		})
	}

	// The keys are compared as Postgres orders them in the "C" collation,
	// the whole row for a table without a primary key, and of schema
	// public only.
	q := strings.Join(src.queries, "\n")
	for _, want := range []string{`collate "C"`, "(t.*)::text", `(row(t."id"))::text`, "n.nspname = 'public'", "indisprimary"} {
		if !strings.Contains(q, want) {
			t.Errorf("no %q in the queries:\n%s", want, q)
		}
	}
}

// TakeKeys and MissingKeys: a key present before and gone now is named.
func TestFingerprintMissingKeys(t *testing.T) {
	db := newFPDB()
	before, err := TakeKeys(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	db.keys["documents"] = []string{"(d1)", "(d3)", "(d4)"}
	now, err := TakeKeys(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got := MissingKeys(before, now); !slices.Equal(got, []string{"documents (d2)"}) {
		t.Errorf("missing %q", got)
	}
	if got := MissingKeys(before, before); len(got) != 0 {
		t.Errorf("nothing gone, yet %q", got)
	}
}

// Native's data size is its database's files, any instance moved aside
// included (ruling R10); nothing there is 0.
func TestNativeDataSize(t *testing.T) {
	n := newNative(t, nil)
	if got, err := n.DataSize(context.Background()); err != nil || got != 0 {
		t.Fatalf("nothing there: %d %v", got, err)
	}
	inst := n.InstanceDir()
	for p, size := range map[string]int{filepath.Join(inst, "data", "PG_VERSION"): 3, filepath.Join(inst, "data", "base", "1"): 1000,
		inst + ".refused-1700000000/data/x": 20, filepath.Join(n.Root(), "venv", "big"): 5000} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := n.DataSize(context.Background()); err != nil || got != 1023 {
		t.Errorf("size %d %v, want 1023 (the venv is not data)", got, err)
	}
}
