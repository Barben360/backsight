package metrics

import (
	"slices"
	"strings"
	"time"
)

// Markers are the release and config markers of w, in time order (a
// release marker before a config marker recorded at the same instant, then
// the rows' file order). They are facts of this PC, not of a project: a
// project filter does not cut them.
//
// A release marker is a prompt whose binary version differs from the
// version of the prompt just before it on this PC; Previous is that
// version, even when that prompt is before the window (a marker must itself
// fall in the window; the first prompt ever is none). A config marker is a
// config record whose hash differs from the previous record's, with the
// numeric settings that differ; a setting absent from one of the two has a
// null From (new) or To (gone). The window's bounds are in the SQL; the
// previous row is read past the lower bound.
func Markers(e env, w Window) ([]Marker, error) {
	out := []Marker{}
	rel, err := releaseMarkers(e, w)
	if err != nil {
		return nil, err
	}
	cfg, err := configMarkers(e, w)
	if err != nil {
		return nil, err
	}
	out = append(out, rel...)
	out = append(out, cfg...)
	slices.SortStableFunc(out, func(a, b Marker) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(b.Kind, a.Kind) // "release" before "config"
	})
	return out, nil
}

func releaseMarkers(e env, w Window) ([]Marker, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
WITH p AS (
  SELECT ts, source_file, "offset", binary_version AS v,
         LAG(binary_version) OVER (ORDER BY ts, source_file, "offset") AS prev
  FROM tel_prompt WHERE ts < ? AND COALESCE(binary_version, '') <> '')
SELECT ts, v, prev FROM p
WHERE ts >= ? AND prev IS NOT NULL AND prev <> v
ORDER BY ts, source_file, "offset"`, w.Until.UnixMilli(), w.Since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Marker
	for rows.Next() {
		var ts int64
		var v, prev string
		if err := rows.Scan(&ts, &v, &prev); err != nil {
			return nil, err
		}
		out = append(out, Marker{At: time.UnixMilli(ts).In(viewLocOf(w)), Kind: "release", Version: v, Previous: prev})
	}
	return out, rows.Err()
}

func configMarkers(e env, w Window) ([]Marker, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
WITH c AS (
  SELECT ts, source_file, "offset", COALESCE(sha256, '') AS sha,
         LAG(COALESCE(sha256, '')) OVER (ORDER BY ts, source_file, "offset") AS prev_sha,
         LAG(source_file) OVER (ORDER BY ts, source_file, "offset") AS prev_file,
         LAG("offset") OVER (ORDER BY ts, source_file, "offset") AS prev_offset
  FROM tel_config WHERE ts < ?)
SELECT ts, source_file, "offset", prev_file, prev_offset FROM c
WHERE ts >= ? AND prev_file IS NOT NULL AND prev_sha <> sha
ORDER BY ts, source_file, "offset"`, w.Until.UnixMilli(), w.Since.UnixMilli())
	if err != nil {
		return nil, err
	}
	type ref struct {
		ts           int64
		file, prevF  string
		off, prevOff int64
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.ts, &r.file, &r.off, &r.prevF, &r.prevOff); err != nil {
			_ = rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	values := func(file string, off int64) (map[string]float64, error) {
		vr, err := e.tx.QueryContext(e.ctx, `SELECT key, value FROM tel_config_values WHERE source_file = ? AND "offset" = ?`, file, off)
		if err != nil {
			return nil, err
		}
		defer func() { _ = vr.Close() }()
		m := map[string]float64{}
		for vr.Next() {
			var k string
			var v float64
			if err := vr.Scan(&k, &v); err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, vr.Err()
	}
	var out []Marker
	for _, r := range refs {
		to, err := values(r.file, r.off)
		if err != nil {
			return nil, err
		}
		from, err := values(r.prevF, r.prevOff)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(to)+len(from))
		for k := range to {
			keys = append(keys, k)
		}
		for k := range from {
			if _, both := to[k]; !both {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		changes := []SettingChange{}
		for _, k := range keys {
			f, hadFrom := from[k]
			t, hasTo := to[k]
			if hadFrom && hasTo && f == t {
				continue
			}
			c := SettingChange{Key: k}
			if hadFrom {
				c.From = ptr(f)
			}
			if hasTo {
				c.To = ptr(t)
			}
			changes = append(changes, c)
		}
		out = append(out, Marker{At: time.UnixMilli(r.ts).In(viewLocOf(w)), Kind: "config", Changes: changes})
	}
	return out, nil
}

func viewLocOf(w Window) *time.Location {
	if w.Loc == nil {
		return time.UTC
	}
	return w.Loc
}
