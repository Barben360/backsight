package termstyle

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var roles = []Role{Brand, Accent, OK, Warn, Fail, Label, Secondary, Command}

func at(lvl Level) *Writer {
	return NewWriter(&bytes.Buffer{}, func() Level { return lvl })
}

func TestPaintOffIsIdentity(t *testing.T) {
	for _, r := range roles {
		if got := Paint(&bytes.Buffer{}, r, "ok"); got != "ok" {
			t.Errorf("Paint(buffer, %d) = %q", r, got)
		}
		if got := Paint(at(None), r, "ok"); got != "ok" {
			t.Errorf("Paint(None, %d) = %q", r, got)
		}
	}
}

func TestPaintLevels(t *testing.T) {
	if got := Paint(at(TrueColor), Brand, "backsight"); got != "\x1b[1;38;2;201;80;122mbacksight\x1b[0m" {
		t.Errorf("brand truecolor = %q", got)
	}
	if got := Paint(at(Ansi256), Brand, "b"); !strings.HasPrefix(got, "\x1b[1;38;5;") {
		t.Errorf("brand 256 = %q", got)
	}
	if got := Paint(at(Basic), Brand, "b"); !strings.HasPrefix(got, "\x1b[1;") || strings.Contains(got, "38;") {
		t.Errorf("brand basic = %q", got)
	}
	for _, lvl := range []Level{Basic, Ansi256, TrueColor} {
		for _, r := range roles {
			got := Paint(at(lvl), r, "x y")
			if got == "x y" {
				t.Errorf("role %d at level %d not painted", r, lvl)
			}
			if Strip(got) != "x y" {
				t.Errorf("Strip(Paint(%d, %d)) = %q", lvl, r, Strip(got))
			}
		}
	}
}

func TestPaintEmpty(t *testing.T) {
	if got := Paint(at(TrueColor), OK, ""); got != "" {
		t.Errorf("Paint empty = %q", got)
	}
}

func TestWriterResolvesOnceAndLazily(t *testing.T) {
	calls := 0
	var buf bytes.Buffer
	w := NewWriter(&buf, func() Level { calls++; return Basic })
	if calls != 0 {
		t.Fatalf("resolved at construction")
	}
	_, _ = w.Write([]byte("a\x00b"))
	_, _ = io.WriteString(w, "c\n")
	for range 3 {
		if w.Level() != Basic {
			t.Fatal("level")
		}
	}
	if calls != 1 {
		t.Errorf("resolved %d times, want 1", calls)
	}
	if buf.String() != "a\x00bc\n" {
		t.Errorf("bytes changed: %q", buf.String())
	}
}

func TestRawAndLevelOf(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inner := NewWriter(f, func() Level { return Basic })
	outer := NewWriter(inner, func() Level { return TrueColor })
	if Raw(outer) != io.Writer(f) {
		t.Errorf("Raw did not unwrap to the file")
	}
	if Raw(io.Discard) != io.Discard {
		t.Errorf("Raw changed a plain writer")
	}
	if LevelOf(io.Discard) != None || LevelOf(outer) != TrueColor {
		t.Errorf("LevelOf")
	}
	if outer.Unwrap() != io.Writer(inner) {
		t.Errorf("Unwrap")
	}
}

func TestStrip(t *testing.T) {
	if got := Strip("\x1b[1;38;2;1;2;3ma\x1b[0m b"); got != "a b" {
		t.Errorf("Strip = %q", got)
	}
	if got := Strip("plain [text] 1;2m"); got != "plain [text] 1;2m" {
		t.Errorf("Strip changed plain text: %q", got)
	}
}
