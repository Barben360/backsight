package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// logHarness is a data harness whose log is the returned buffer.
func logHarness(t *testing.T) (*harness, *lockedBuffer) {
	t.Helper()
	log := &lockedBuffer{}
	h := newDataHarness(t, withIndex(), withServer(), withDoctor(), func(_ *harness, c *Config) { c.Log = log })
	return h, log
}

// A page whose view fails is the failure page: the status the API gives, the fixed sentence (never the
// error's own text, which may name a path), the page's title; once the view reads again the page is served,
// so a failure is not kept.
func TestAPageWhoseViewFailsIsTheFailurePage(t *testing.T) {
	h, log := logHarness(t)
	ck := cookieHdr(h.login())
	h.ix.set(func(f *fakeIndex) { f.readErr = errors.New("read /home/someone/private/metrics.db: disk exploded") })
	resp := h.do("GET", "/quality", ck...)
	b := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET /quality with a failing view: %d, want 500", resp.StatusCode)
	}
	text := visibleText(b)
	if !strings.Contains(text, sentence(viewFailsSentence)) {
		t.Errorf("no %q on the failure page: %.400s", sentence(viewFailsSentence), text)
	}
	if strings.Contains(b, "someone") || strings.Contains(b, "exploded") {
		t.Errorf("the error's own text reached the page: %.400s", text)
	}
	if strings.Contains(b, `id="w-used-turn_rate"`) {
		t.Error("the failure page draws the page's cards")
	}
	if got := log.String(); got != "" {
		t.Errorf("an error the page reports is logged: %q", got)
	}
	h.ix.set(func(f *fakeIndex) { f.readErr = nil })
	if resp := h.do("GET", "/quality", ck...); resp.StatusCode != http.StatusOK || !strings.Contains(body(t, resp), `id="w-used-turn_rate"`) {
		t.Errorf("GET /quality once the view reads again: %d", resp.StatusCode)
	}
}

// A view that panics while it reads (in the computation the result cache runs apart from the request) fails
// its page and its API answer, not the dashboard: the page is the failure page, the dashboard serves the next
// request, and the log says which view panicked and where. The panic's value is a runtime error's own message
// or only its type: a value of the code's could hold what a view read (a memory's text, a path).
func TestAViewThatPanicsIsLoggedAndTheDashboardServesOn(t *testing.T) {
	h, log := logHarness(t)
	ck := cookieHdr(h.login())
	const canary = "CANARY-MEMORY-TEXT"
	h.ix.set(func(f *fakeIndex) { f.readPanic = "a memory read: " + canary })
	resp := h.do("GET", "/quality", ck...)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(visibleText(body(t, resp)), sentence(viewFailsSentence)) {
		t.Fatalf("GET /quality with a panicking view: %d, want 500 and the failure sentence", resp.StatusCode)
	}
	if resp := h.do("GET", "/api/v1/quality", ck...); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET /api/v1/quality with a panicking view: %d, want 500", resp.StatusCode)
	}
	got := log.String()
	for _, want := range []string{"dashboard: the quality view panicked (a value of type string)", "goroutine ", "internal/dashboard.(*fakeIndex).ReadTx"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, canary) {
		t.Errorf("the panic's value reached the log:\n%s", got)
	}

	// A runtime error says what it is: its message is the runtime's, with no data of the view's.
	h.ix.set(func(f *fakeIndex) { f.readPanic = runtimeError(t) })
	if resp := h.do("GET", "/cost", ck...); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET /cost with a panicking view: %d, want 500", resp.StatusCode)
	}
	if got := log.String(); !strings.Contains(got, "dashboard: the cost view panicked: runtime error: index out of range [3] with length 2") {
		t.Errorf("the runtime error is not in the log:\n%s", got)
	}

	h.ix.set(func(f *fakeIndex) { f.readPanic = nil })
	if resp := h.do("GET", "/quality", ck...); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /quality once the view reads again: %d", resp.StatusCode)
	}
}

// runtimeError is the value of an index out of range.
func runtimeError(t *testing.T) (v any) {
	t.Helper()
	defer func() { v = recover() }()
	xs, i := []int{1, 2}, 3
	_ = xs[i]
	t.Fatal("no panic")
	return nil
}
