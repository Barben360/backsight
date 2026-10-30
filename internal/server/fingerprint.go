package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The runtime switch's gate (decision 0078): the data, exactly. A
// fingerprint is taken on the source right after its dump (its API
// stopped, nothing written since) and on the target right after the
// restore, before its API starts; any difference fails the switch.

// TableSum is one table: its rows and an md5 of its keys.
type TableSum struct {
	Rows int64  `json:"rows"`
	Keys string `json:"keys_md5"` // md5 of the primary-key values in key order; of the whole row text for a table without one
}

// Fingerprint is what the gate compares: every base table of schema
// public, the bank list and the control bank's documents (server_id,
// epoch, feedback ledgers).
type Fingerprint struct {
	Tables  map[string]TableSum `json:"tables"`            // every base table of schema public
	Banks   []string            `json:"banks"`             //
	Control map[string]string   `json:"control_documents"` // <bank>-control document id → md5(retain_params::jsonb::text)
}

// fpTable is a base table of schema public and its primary key's columns
// in key order (none: compared by whole rows).
type fpTable struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// The queries carry a comment naming what they read: a fake database
// answers by it.
const fpTablesSQL = `/* backsight:tables */ select coalesce(json_agg(json_build_object('name', c.relname, 'keys', coalesce((` +
	`select json_agg(a.attname order by k.ord) from pg_index i cross join lateral unnest(i.indkey::int2[]) with ordinality as k(attnum, ord) ` +
	`join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum where i.indrelid = c.oid and i.indisprimary), '[]'::json)) order by c.relname), '[]'::json) ` +
	`from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname = 'public' and c.relkind in ('r', 'p') and not c.relispartition`

// quoteIdent is name as an SQL identifier.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// keyExpr is the text of one row's key: its primary key's values as a
// row, else the whole row. Keys are ordered by that text in the "C"
// collation, byte by byte, so two Postgres builds with other default
// collations (the image's and pg0's) order them alike.
func keyExpr(t fpTable) string {
	if len(t.Keys) == 0 {
		return "(t.*)::text"
	}
	cols := make([]string, len(t.Keys))
	for i, k := range t.Keys {
		cols[i] = "t." + quoteIdent(k)
	}
	return "(row(" + strings.Join(cols, ", ") + "))::text"
}

// fpTableSQL sums one table: "<rows> <md5>".
func fpTableSQL(t fpTable) string {
	return "/* backsight:table " + t.Name + " */ select count(*) || ' ' || md5(coalesce(string_agg(k, E'\\n' order by k collate \"C\"), '')) from (select " +
		keyExpr(t) + " as k from public." + quoteIdent(t.Name) + " t) s"
}

// fpKeysSQL lists one table's keys as a JSON array, in key order.
func fpKeysSQL(t fpTable) string {
	return "/* backsight:keys " + t.Name + " */ select coalesce(json_agg(k order by k collate \"C\"), '[]'::json) from (select " +
		keyExpr(t) + " as k from public." + quoteIdent(t.Name) + " t) s"
}

// fpControlSQL hashes each document of the control bank of bank
// (retain.ControlBank: <bank>-control) by its retain_params, which hold
// its metadata (Hindsight 1.1.0).
func fpControlSQL(bank string) string {
	return "/* backsight:control */ select coalesce(json_object_agg(id, md5(retain_params::jsonb::text) order by id), '{}'::json) from documents where bank_id = " +
		sqlString(bank+"-control")
}

func fpTables(ctx context.Context, db Database) ([]fpTable, error) {
	out, err := db.Query(ctx, fpTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("listing the tables: %w", err)
	}
	var tables []fpTable
	if err := json.Unmarshal([]byte(out), &tables); err != nil {
		return nil, fmt.Errorf("listing the tables: %q: %w", out, err)
	}
	return tables, nil
}

// TakeFingerprint reads db's fingerprint for bank, read only.
func TakeFingerprint(ctx context.Context, db Database, bank string) (Fingerprint, error) {
	fp := Fingerprint{Tables: map[string]TableSum{}, Control: map[string]string{}}
	tables, err := fpTables(ctx, db)
	if err != nil {
		return fp, err
	}
	for _, t := range tables {
		out, err := db.Query(ctx, fpTableSQL(t))
		if err != nil {
			return fp, fmt.Errorf("summing %s: %w", t.Name, err)
		}
		rows, sum, ok := strings.Cut(strings.TrimSpace(out), " ")
		n, nerr := strconv.ParseInt(rows, 10, 64)
		if !ok || nerr != nil || len(sum) != 32 {
			return fp, fmt.Errorf("summing %s: %q", t.Name, out)
		}
		fp.Tables[t.Name] = TableSum{Rows: n, Keys: sum}
	}
	if fp.Banks, err = db.BankIDs(ctx); err != nil {
		return fp, fmt.Errorf("listing the banks: %w", err)
	}
	if fp.Banks == nil {
		fp.Banks = []string{}
	}
	out, err := db.Query(ctx, fpControlSQL(bank))
	if err != nil {
		return fp, fmt.Errorf("reading the control documents: %w", err)
	}
	if err := json.Unmarshal([]byte(out), &fp.Control); err != nil {
		return fp, fmt.Errorf("reading the control documents: %q: %w", out, err)
	}
	return fp, nil
}

// TakeKeys lists every table's keys (a table without a primary key: its
// rows' text), for a later comparison of what is gone (decision 0078's
// round trip).
func TakeKeys(ctx context.Context, db Database) (map[string][]string, error) {
	tables, err := fpTables(ctx, db)
	if err != nil {
		return nil, err
	}
	keys := map[string][]string{}
	for _, t := range tables {
		out, err := db.Query(ctx, fpKeysSQL(t))
		if err != nil {
			return nil, fmt.Errorf("listing the keys of %s: %w", t.Name, err)
		}
		var list []string
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return nil, fmt.Errorf("listing the keys of %s: %w", t.Name, err)
		}
		keys[t.Name] = list
	}
	return keys, nil
}

// MissingKeys names, table by table, the keys of before that now lacks.
func MissingKeys(before, now map[string][]string) []string {
	var out []string
	for _, t := range slices.Sorted(maps.Keys(before)) {
		have := map[string]bool{}
		for _, k := range now[t] {
			have[k] = true
		}
		for _, k := range before[t] {
			if !have[k] {
				out = append(out, t+" "+k)
			}
		}
	}
	return out
}

// Diff lists what differs from a to b, empty when they are equal:
// "documents: rows 120 → 119", "documents: keys differ", "control
// document backsight:epoch differs", "bank x missing", …
func (a Fingerprint) Diff(b Fingerprint) []string {
	var out []string
	for _, t := range slices.Sorted(maps.Keys(a.Tables)) {
		x := a.Tables[t]
		y, ok := b.Tables[t]
		switch {
		case !ok:
			out = append(out, "table "+t+" missing")
		case x.Rows != y.Rows:
			out = append(out, fmt.Sprintf("%s: rows %d → %d", t, x.Rows, y.Rows))
		case x.Keys != y.Keys:
			out = append(out, t+": keys differ")
		}
	}
	for _, t := range slices.Sorted(maps.Keys(b.Tables)) {
		if _, ok := a.Tables[t]; !ok {
			out = append(out, "table "+t+" added")
		}
	}
	for _, bk := range a.Banks {
		if !slices.Contains(b.Banks, bk) {
			out = append(out, "bank "+bk+" missing")
		}
	}
	for _, bk := range b.Banks {
		if !slices.Contains(a.Banks, bk) {
			out = append(out, "bank "+bk+" added")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(a.Control)) {
		y, ok := b.Control[id]
		switch {
		case !ok:
			out = append(out, "control document "+id+" missing")
		case y != a.Control[id]:
			out = append(out, "control document "+id+" differs")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(b.Control)) {
		if _, ok := a.Control[id]; !ok {
			out = append(out, "control document "+id+" added")
		}
	}
	return out
}
