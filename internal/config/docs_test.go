package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// docs/configuration.md documents every config.toml key with its default:
// these tests keep it in step with the Config struct and Default, so a new,
// renamed or re-defaulted key fails here until the reference says so.

const configDoc = "../../docs/configuration.md"

// docDirs and docHome are the placeholders the reference writes defaults
// with: "<data>/backups", and the home directory's name as a transverse alias.
var (
	docDirs = paths.Dirs{Data: "<data>", Config: "<config>", State: "<state>"}
	docHome = "/home/<user>"
)

// tomlKeys returns every key of v's struct type as its dotted TOML path,
// with the value Default gives it.
func tomlKeys(prefix string, v reflect.Value) map[string]reflect.Value {
	out := map[string]reflect.Value{}
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		key := prefix + tag
		if f := v.Field(i); f.Kind() == reflect.Struct {
			for k, fv := range tomlKeys(key+".", f) {
				out[k] = fv
			}
		} else {
			out[key] = f
		}
	}
	return out
}

// literal renders a default as the reference writes it: TOML syntax.
func literal(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return strconv.Quote(filepath.ToSlash(v.String())) // the reference writes "<data>/backups" on every OS
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float64:
		s := strconv.FormatFloat(v.Float(), 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	case reflect.Slice:
		var items []string
		for i := range v.Len() {
			items = append(items, literal(v.Index(i)))
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	panic(fmt.Sprintf("no literal for %s", v.Kind()))
}

// docRow is one key row of the reference: | `key` | `default` | effect |.
var docRow = regexp.MustCompile("^\\|\\s*`([a-z_]+(?:\\.[a-z_]+)?)`\\s*\\|\\s*(.*?)\\s*\\|\\s*(.*?)\\s*\\|\\s*$")

type docKey struct {
	line          int
	value, effect string
}

func docKeys(md string) (map[string]docKey, []string) {
	keys := map[string]docKey{}
	var dups []string
	for i, line := range strings.Split(md, "\n") {
		m := docRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, ok := keys[m[1]]; ok {
			dups = append(dups, m[1])
		}
		keys[m[1]] = docKey{i + 1, m[2], m[3]}
	}
	return keys, dups
}

func checkReference(md string) []string {
	want := tomlKeys("", reflect.ValueOf(Default(docDirs, docHome)))
	got, dups := docKeys(md)
	var errs []string
	for _, d := range dups {
		errs = append(errs, fmt.Sprintf("%s is documented twice", d))
	}
	for key, v := range want {
		row, ok := got[key]
		def := "`" + literal(v) + "`"
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf("%s is not documented; add: | `%s` | %s | <what it does> |", key, key, def))
		case row.value != def:
			errs = append(errs, fmt.Sprintf("line %d: %s defaults to %s, the reference says %s", row.line, key, def, row.value))
		case len(row.effect) < 10:
			errs = append(errs, fmt.Sprintf("line %d: %s has no description", row.line, key))
		}
	}
	for key, row := range got {
		if _, ok := want[key]; !ok {
			errs = append(errs, fmt.Sprintf("line %d: %s is not a config.toml key", row.line, key))
		}
	}
	slices.Sort(errs)
	return errs
}

func TestConfigReferenceDocumentsEveryKeyWithItsDefault(t *testing.T) {
	raw, err := os.ReadFile(configDoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range checkReference(string(raw)) {
		t.Error(e)
	}
}

// The check itself: a missing key, a wrong default, an unknown key and a
// duplicate are each reported.
func TestCheckReferenceReportsDrift(t *testing.T) {
	var full strings.Builder
	for key, v := range tomlKeys("", reflect.ValueOf(Default(docDirs, docHome))) {
		fmt.Fprintf(&full, "| `%s` | `%s` | what this key does |\n", key, literal(v))
	}
	if errs := checkReference(full.String()); len(errs) != 0 {
		t.Fatalf("a complete reference is refused: %q", errs)
	}
	for name, c := range map[string]struct {
		edit func(string) string
		want string
	}{
		"missing": {func(s string) string {
			return regexp.MustCompile("(?m)^\\| `recall.min_gap` .*\n").ReplaceAllString(s, "")
		},
			"recall.min_gap is not documented; add: | `recall.min_gap` | `3` | <what it does> |"},
		"wrong default": {func(s string) string {
			return strings.Replace(s, "| `recall.min_gap` | `3` |", "| `recall.min_gap` | `4` |", 1)
		},
			"defaults to `3`, the reference says `4`"},
		"unknown": {func(s string) string { return s + "| `recall.min_gaps` | `3` | what this key does |\n" },
			"recall.min_gaps is not a config.toml key"},
		"duplicate": {func(s string) string { return s + "| `backup.dir` | `\"<data>/backups\"` | what this key does |\n" },
			"backup.dir is documented twice"},
		"no description": {func(s string) string {
			return strings.Replace(s, "| `backup.dir` | `\"<data>/backups\"` | what this key does |", "| `backup.dir` | `\"<data>/backups\"` |  |", 1)
		},
			"backup.dir has no description"},
	} {
		errs := checkReference(c.edit(full.String()))
		if len(errs) != 1 || !strings.Contains(errs[0], c.want) {
			t.Errorf("%s: got %q, want one error with %q", name, errs, c.want)
		}
	}
}

// Every TOML example in the reference is a valid config.toml.
func TestConfigReferenceExamplesParse(t *testing.T) {
	testtool.SkipWindows(t, "Task 11: docs/configuration.md examples hold POSIX paths")
	raw, err := os.ReadFile(configDoc)
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```toml\n(.*?)```").FindAllStringSubmatch(string(raw), -1)
	if len(blocks) == 0 {
		t.Fatal("no ```toml example in the reference")
	}
	for i, b := range blocks {
		if _, err := Parse([]byte("schema_version = 1\n"+b[1]), fmt.Sprintf("example %d", i+1), dirs, "/home/u"); err != nil {
			t.Errorf("example %d: %v", i+1, err)
		}
	}
}
