package dashboard

import (
	"html"
	"strings"
	"testing"
)

// On a PC with no memory server set up, every card says so, as the scope
// line and the JSON (server.error not_configured) do: none says the
// server is "not answering", nor that the memory is on a remote server.
func TestNoServerSetUpIsNotAServerDown(t *testing.T) {
	h := newDataHarness(t, withIndex(), withDoctor())
	hdr := cookieHdr(h.login())
	read := func(path string) string {
		t.Helper()
		r := h.do("GET", path, hdr...)
		raw := body(t, r)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		}
		return html.UnescapeString(raw)
	}
	page := read("/")
	for _, want := range []string{
		noServerSentence, // Memory at a glance
		"No memory server is set up on this PC: the counts are from this PC, and no text can be read.", // Helped most
		"(no memory server set up).",                        // the summary line
		"Activity on this PC · no memory server set up",     // Copy summary
		"Memory: size not known (no memory server set up).", // Copy summary
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/ does not say %q", want)
		}
	}
	system := read("/system")
	if want := "No memory server is set up on this PC, so other PCs’ figures are not fetched."; !strings.Contains(system, want) {
		t.Errorf("/system does not say %q", want)
	}
	for path, b := range map[string]string{"/": page, "/system": system} {
		for _, wrong := range []string{"not answering", "remote server"} {
			if strings.Contains(b, wrong) {
				i := strings.Index(b, wrong)
				t.Errorf("%s says %q: …%s…", path, wrong, b[max(0, i-120):min(len(b), i+60)])
			}
		}
	}
}
