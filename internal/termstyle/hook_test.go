package termstyle

import (
	"bytes"
	"io"
	"testing"
)

func TestBeforeFirstWriteRunsOnceBeforeBytes(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, func() Level { return TrueColor })
	calls := 0
	w.BeforeFirstWrite(func(raw io.Writer) {
		calls++
		_, _ = io.WriteString(raw, "[splash]")
	})
	_, _ = io.WriteString(w, "one ")
	_, _ = io.WriteString(w, "two")
	if got := buf.String(); got != "[splash]one two" || calls != 1 {
		t.Fatalf("got %q after %d calls, want the hook's bytes once, first", got, calls)
	}
}

func TestBeforeFirstWriteSkipsEmptyWrite(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, func() Level { return TrueColor })
	calls := 0
	w.BeforeFirstWrite(func(io.Writer) { calls++ })
	_, _ = w.Write(nil)
	if calls != 0 {
		t.Fatalf("an empty write ran the hook")
	}
	_, _ = w.Write([]byte("x"))
	if calls != 1 {
		t.Fatalf("the first real write ran the hook %d times", calls)
	}
}

func TestBeforeFirstWriteSeesResolvedLevel(t *testing.T) {
	resolves := 0
	w := NewWriter(&bytes.Buffer{}, func() Level { resolves++; return Ansi256 })
	var seen Level
	w.BeforeFirstWrite(func(io.Writer) { seen = w.Level() })
	_, _ = w.Write([]byte("x"))
	if seen != Ansi256 || resolves != 1 {
		t.Fatalf("hook saw %v after %d resolves, want Ansi256 resolved once", seen, resolves)
	}
}

func TestLogoShades(t *testing.T) {
	for _, c := range []struct {
		r    Role
		lvl  Level
		want string
	}{
		{ArcMiddle, TrueColor, "\x1b[38;2;172;99;124m"},
		{ArcMiddle, Ansi256, "\x1b[38;5;132m"},
		{ArcMiddle, Basic, "\x1b[35m"},
		{ArcOuter, TrueColor, "\x1b[38;2;150;114;126m"},
		{ArcOuter, Ansi256, "\x1b[38;5;138m"},
		{Dot, TrueColor, "\x1b[1;38;2;232;145;58m"},
		{Dot, Ansi256, "\x1b[1;38;5;208m"},
		{Dot, Basic, "\x1b[1;33m"},
	} {
		if got := sgr(c.r, c.lvl); got != c.want {
			t.Errorf("sgr(%d, %v) = %q, want %q", c.r, c.lvl, got, c.want)
		}
	}
}
