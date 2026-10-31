package dashboard

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// manualTicks makes Serve's idle checks run only when the test sends a
// tick on the channel returned, for the harnesses started after it.
func manualTicks(t *testing.T) chan time.Time {
	t.Helper()
	ticks := make(chan time.Time)
	t.Cleanup(func() { idleTicks = defaultIdleTicks })
	idleTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	return ticks
}

// checkIdle has Serve judge the idle time at the clock's time: true when
// that stopped the dashboard. The second tick is taken only once the
// first check is over without a stop.
func (h *harness) checkIdle(ticks chan time.Time) bool {
	h.t.Helper()
	ctx := context.Background()
	if d, ok := h.t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d)
		defer cancel()
	}
	for range 2 {
		select {
		case ticks <- time.Time{}:
		case <-h.done:
			return true
		case <-ctx.Done():
			h.t.Fatal("Serve neither took a tick nor returned before the test's deadline")
		}
	}
	return false
}

// unauthenticated sends requests that are no session's: a probe, a HEAD
// and a prefetch of a link, a forged cookie.
func (h *harness) unauthenticated() {
	h.t.Helper()
	code := h.mint()
	for _, r := range []*http.Response{
		h.do("GET", "/"),
		h.do("HEAD", "/?code="+code),
		h.do("GET", "/?code="+code, "Sec-Purpose", "prefetch"),
		h.do("GET", "/api/v1/overview", "Cookie", cookieName(h.port)+"=forged"),
	} {
		if r.StatusCode != http.StatusUnauthorized {
			h.t.Fatalf("an unauthenticated request got %d, want 401", r.StatusCode)
		}
	}
}

func wantIdleStop(t *testing.T, h *harness) {
	t.Helper()
	if h.reason != "idle" || h.err != nil {
		t.Errorf("Serve: %q, %v; want idle, nil", h.reason, h.err)
	}
	noRuntimeFile(t, Config{Dirs: h.dirs})
}

func TestIdleStop(t *testing.T) {
	defer func(d time.Duration) { idleCheck = d }(idleCheck)
	idleCheck = time.Millisecond

	t.Run("after the last signed-in request", func(t *testing.T) {
		ticks := manualTicks(t)
		h := newHarness(t)
		at := h.clock.now()
		ck := h.login()
		h.clock.set(at.Add(time.Hour + 59*time.Minute))
		if h.checkIdle(ticks) {
			t.Fatal("stopped 1 h 59 min after a login")
		}
		if r := h.do("GET", "/quality", cookieHdr(ck)...); r.StatusCode != http.StatusOK {
			t.Fatalf("a signed-in request: %d, want 200", r.StatusCode)
		}
		seen := h.clock.now()
		// Two hours after the signed-in request, not more: still serving,
		// whatever unauthenticated requests came meanwhile.
		h.clock.set(seen.Add(2 * time.Hour))
		h.unauthenticated()
		if h.checkIdle(ticks) {
			t.Fatal("stopped 2 h after a signed-in request: the request did not count")
		}
		h.clock.set(seen.Add(2*time.Hour + time.Second))
		h.unauthenticated()
		if !h.checkIdle(ticks) {
			t.Fatal("still serving 2 h 1 s after the last signed-in request: an unauthenticated request counted")
		}
		wantIdleStop(t, h)
	})

	t.Run("a login and a reopened link count", func(t *testing.T) {
		ticks := manualTicks(t)
		h := newHarness(t)
		h.clock.add(time.Hour)
		ck := h.login()
		h.clock.add(2 * time.Hour)
		if h.checkIdle(ticks) {
			t.Fatal("stopped 2 h after a login: the login did not count")
		}
		if r := h.do("GET", "/?code=xyz", cookieHdr(ck)...); r.StatusCode != http.StatusSeeOther {
			t.Fatalf("a spent link reopened: %d, want 303", r.StatusCode)
		}
		h.clock.add(2 * time.Hour)
		if h.checkIdle(ticks) {
			t.Fatal("stopped 2 h after a reopened link: it did not count")
		}
	})

	t.Run("nobody signs in", func(t *testing.T) {
		ticks := manualTicks(t)
		h := newHarness(t)
		start := h.clock.now()
		h.clock.set(start.Add(2 * time.Hour))
		h.unauthenticated()
		if h.checkIdle(ticks) {
			t.Fatal("stopped 2 h after Start")
		}
		h.clock.set(start.Add(2*time.Hour + time.Second))
		if !h.checkIdle(ticks) {
			t.Fatal("still serving 2 h 1 s after Start with nobody signed in")
		}
		wantIdleStop(t, h)
	})

	// The real ticker, every idleCheck: Serve stops on its own. The
	// timeout only bounds a regression's wait: a working check stops it
	// within a few ticks.
	t.Run("ticker", func(t *testing.T) {
		h := newHarness(t)
		h.clock.add(2*time.Hour + time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		select {
		case <-h.done:
		case <-ctx.Done():
			t.Fatal("Serve did not stop within 10 s of ticks every millisecond")
		}
		wantIdleStop(t, h)
	})
}
