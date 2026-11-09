package termstyle

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveOrder(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		env  map[string]string
		want Level
	}{
		{"json beats always", Inputs{JSON: true, Flag: Always, Terminal: true}, map[string]string{"BACKSIGHT_COLOR": "always"}, None},
		{"plain beats always", Inputs{Plain: true, Flag: Always, Terminal: true}, nil, None},
		{"flag never beats terminal", Inputs{Flag: Never, Terminal: true}, nil, None},
		{"flag always off terminal", Inputs{Flag: Always}, nil, Basic},
		{"flag always beats NO_COLOR", Inputs{Flag: Always}, map[string]string{"NO_COLOR": "1"}, Basic},
		{"flag always beats CLAUDECODE", Inputs{Flag: Always, Terminal: true}, map[string]string{"CLAUDECODE": "1"}, Basic},
		{"variable never beats terminal", Inputs{Terminal: true}, map[string]string{"BACKSIGHT_COLOR": "never"}, None},
		{"variable always beats NO_COLOR", Inputs{}, map[string]string{"BACKSIGHT_COLOR": "always", "NO_COLOR": "1"}, Basic},
		{"explicit auto falls through to NO_COLOR", Inputs{Flag: Auto, Terminal: true}, map[string]string{"BACKSIGHT_COLOR": "auto", "NO_COLOR": "1"}, None},
		{"invalid variable ignored", Inputs{Terminal: true}, map[string]string{"BACKSIGHT_COLOR": "rainbow"}, Basic},
		{"NO_COLOR empty is unset", Inputs{Terminal: true}, map[string]string{"NO_COLOR": ""}, Basic},
		{"auto needs a terminal", Inputs{}, nil, None},
		{"TERM dumb", Inputs{Terminal: true}, map[string]string{"TERM": "dumb"}, None},
		{"CLAUDECODE turns auto off", Inputs{Terminal: true}, map[string]string{"CLAUDECODE": "1"}, None},
		{"FORCE_COLOR ignored", Inputs{}, map[string]string{"FORCE_COLOR": "1", "CLICOLOR_FORCE": "1"}, None},
		{"auto at a terminal", Inputs{Terminal: true}, map[string]string{"TERM": "xterm-256color"}, Ansi256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Getenv = envOf(c.env)
			if got := Resolve(in); got != c.want {
				t.Errorf("Resolve = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveNilGetenv(t *testing.T) {
	for k, v := range map[string]string{"CLAUDECODE": "", "NO_COLOR": "", "BACKSIGHT_COLOR": "", "COLORTERM": "", "WT_SESSION": "", "TERM": "xterm"} {
		t.Setenv(k, v)
	}
	if got := Resolve(Inputs{Terminal: true}); got != Basic {
		t.Errorf("Resolve with nil Getenv = %v, want Basic", got)
	}
}

func TestDetectLevel(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want Level
	}{
		{map[string]string{"COLORTERM": "truecolor"}, TrueColor},
		{map[string]string{"COLORTERM": "24bit"}, TrueColor},
		{map[string]string{"TERM": "xterm-256color"}, Ansi256},
		{map[string]string{"TERM": "xterm"}, Basic},
		{map[string]string{"TERM": ""}, Basic},
		{map[string]string{"WT_SESSION": "x"}, TrueColor},
		{map[string]string{"COLORTERM": "truecolor", "TERM": "dumb"}, TrueColor},
	}
	for _, c := range cases {
		if got := DetectLevel(envOf(c.env)); got != c.want {
			t.Errorf("DetectLevel(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for s, want := range map[string]Mode{"auto": Auto, "always": Always, "never": Never} {
		got, err := ParseMode(s)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", s, got, err)
		}
		if got.String() != s {
			t.Errorf("Mode(%d).String() = %q, want %q", got, got.String(), s)
		}
	}
	for _, s := range []string{"Always", "", "rainbow"} {
		if _, err := ParseMode(s); err == nil || !strings.Contains(err.Error(), "want auto, always or never") {
			t.Errorf("ParseMode(%q) err = %v", s, err)
		}
	}
}
