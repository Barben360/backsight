package dashboard

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// block is the text between the braces that follow the first occurrence of
// head in css, nested braces included.
func block(t testing.TB, css, head string) string {
	t.Helper()
	i := strings.Index(css, head)
	if i < 0 {
		t.Fatalf("tokens.css has no %q", head)
	}
	start := strings.IndexByte(css[i:], '{') + i + 1
	depth := 1
	for j := start; j < len(css); j++ {
		switch css[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return css[start:j]
			}
		}
	}
	t.Fatalf("tokens.css: %q is never closed", head)
	return ""
}

var tokenDecl = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)

// props are the custom properties a block declares.
func props(body string) map[string]string {
	out := map[string]string{}
	for _, m := range tokenDecl.FindAllStringSubmatch(cssComment.ReplaceAllString(body, ""), -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

func tokensCSS(t testing.TB) string {
	t.Helper()
	b, err := assetFS.ReadFile("assets/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tokenThemes are the light and dark properties of tokens.css.
func tokenThemes(t testing.TB) (light, dark map[string]string) {
	css := tokensCSS(t)
	return props(block(t, css, ":root,\n[data-theme=\"light\"]")), props(block(t, css, "[data-theme=\"dark\"] {"))
}

func lum(hex string) float64 {
	n, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil || len(hex) != 7 {
		panic("not a #RRGGBB colour: " + hex)
	}
	ch := func(v uint64) float64 {
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(n>>16&255) + 0.7152*ch(n>>8&255) + 0.0722*ch(n&255)
}

// ratio is the WCAG 2.x contrast ratio of two #RRGGBB colours.
func ratio(a, b string) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastOfTokens(t *testing.T) {
	light, dark := tokenThemes(t)
	for name, th := range map[string]map[string]string{"light": light, "dark": dark} {
		check := func(fg, bg string, min float64) {
			t.Helper()
			f, b := th[fg], th[bg]
			if f == "" || b == "" {
				t.Errorf("%s: %s or %s is not declared", name, fg, bg)
				return
			}
			if r := ratio(f, b); r < min {
				t.Errorf("%s: %s %s on %s %s is %.2f, want at least %.1f", name, fg, f, bg, b, r, min)
			}
		}
		for _, ink := range []string{"--fg", "--fg-2", "--fg-3", "--link", "--accent"} {
			for _, bg := range []string{"--surface", "--bg", "--surface-2"} {
				check(ink, bg, 4.5)
			}
		}
		for _, st := range []string{"--ok", "--watch", "--bad", "--neutral", "--learn"} {
			for _, bg := range []string{"--surface", "--bg", "--surface-2", st + "-bg"} {
				check(st, bg, 4.5)
			}
		}
		// The sequential steps --seq-1..3 are exempt (design README: the
		// legend, the gaps and the table make up for them).
		for _, c := range []string{"--c-used", "--c-ignored", "--c-undeclared", "--c-shown", "--k-summary", "--k-about", "--k-session",
			"--p-ok", "--p-queued", "--p-aside", "--p-failed"} {
			check(c, "--surface", 3)
		}
	}
	for _, a := range []struct {
		theme  map[string]string
		name   string
		fg, bg string
		want   string
	}{
		{light, "light", "--fg", "--surface", "15.80"},
		{light, "light", "--accent", "--surface-2", "4.64"},
		{dark, "dark", "--fg", "--surface", "14.64"},
		{dark, "dark", "--p-failed", "--surface", "7.62"},
	} {
		if got := fmt.Sprintf("%.2f", ratio(a.theme[a.fg], a.theme[a.bg])); got != a.want {
			t.Errorf("%s %s on %s is %s, the design says %s", a.name, a.fg, a.bg, got, a.want)
		}
	}
}

func TestContrastRatioMath(t *testing.T) {
	if got := ratio("#000000", "#FFFFFF"); math.Abs(got-21) > 1e-9 {
		t.Errorf("black on white %.4f, want 21", got)
	}
	if got := ratio("#777777", "#777777"); got != 1 {
		t.Errorf("same colour %.4f, want 1", got)
	}
	if got := ratio("#767676", "#FFFFFF"); got < 4.5 || got > 4.6 {
		t.Errorf("#767676 on white %.4f, want about 4.54", got)
	}
}

func TestDarkPaletteStatedTwiceTheSame(t *testing.T) {
	css := tokensCSS(t)
	media := props(block(t, block(t, css, "@media (prefers-color-scheme: dark)"), ":root:not([data-theme=\"light\"])"))
	attr := props(block(t, css, "[data-theme=\"dark\"] {"))
	if len(attr) < 40 {
		t.Fatalf("only %d dark properties read", len(attr))
	}
	for k, v := range attr {
		if media[k] != v {
			t.Errorf("%s: %q under prefers-color-scheme, %q under data-theme=dark", k, media[k], v)
		}
	}
	for k := range media {
		if _, ok := attr[k]; !ok {
			t.Errorf("%s is only in the prefers-color-scheme block", k)
		}
	}
}
