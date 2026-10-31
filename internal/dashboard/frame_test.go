package dashboard

import (
	"strings"
	"testing"
)

func TestFrameLinks(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/quality?days=30&project=p1")
	for _, want := range []string{
		`<a href="/quality?days=1&project=p1">24 h</a>`,
		`<a href="/quality?days=7&project=p1">7 d</a>`,
		`<a href="/quality?days=30&project=p1" aria-current="page">30 d</a>`,
		`<a href="/quality?days=90&project=p1">90 d</a>`,
		`<a href="/quality?days=30&project=p1">Refresh</a>`,
		`<a href="/prefs?theme=dark&back=%2Fquality%3Fdays%3D30%26project%3Dp1">Dark</a>`,
		`<a href="/quality?days=30">All projects</a>`,
		`<input type="text" name="project" value="p1">`,
		`<input type="hidden" name="days" value="30">`,
		`<form method="get" action="/quality">`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	if strings.Contains(b, "<summary>Developer</summary>") {
		t.Error("Developer menu without the preference")
	}
}

func TestFrameCustomWindowIsKeptOnRefresh(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/quality?since=2026-10-01&until=2026-10-08")
	if !strings.Contains(b, `<a href="/quality?since=2026-10-01&until=2026-10-08">Refresh</a>`) || strings.Contains(b, `aria-current="page">30 d`) {
		t.Errorf("custom window: %s", b)
	}
}

func TestReflectedParamsAreEscaped(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/quality?project=%3Cscript%3Ex%3C%2Fscript%3E", cookieHdr(h.login())...)
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "&lt;script&gt;x") || strings.Contains(b, "<script>x") {
		t.Errorf("%d %s", r.StatusCode, b)
	}
}

func TestDefaultWindowIsMarked(t *testing.T) {
	h := dataHarness(t)
	_, b := pageGet(t, h, h.login(), "/quality?tz=UTC&project=a&project=b")
	for _, want := range []string{
		`aria-current="page">7 d</a>`,
		`<a href="/quality?days=1&tz=UTC&project=a">24 h</a>`,
		`value="a"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	if strings.Contains(b, `aria-current="page">30 d`) {
		t.Error("30 d marked")
	}
}

func TestReflectedQuoteIsEscapedInAttributes(t *testing.T) {
	h := dataHarness(t)
	r := h.do("GET", "/quality?project=%22%3E%3Cb%3Ex", cookieHdr(h.login())...)
	b := body(t, r)
	if r.StatusCode != 200 || strings.Contains(b, `"><b>x`) || strings.Contains(b, "<b>x") || !strings.Contains(b, `value="&#34;&gt;&lt;b&gt;x"`) {
		t.Errorf("%d %s", r.StatusCode, b)
	}
}
