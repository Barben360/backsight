package dashboard

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func newTestSessions(t *testing.T) (*sessions, *fakeClock) {
	t.Helper()
	c := &fakeClock{t: t0}
	return newSessions(c.now), c
}

func mustCreate(t *testing.T, s *sessions) string {
	t.Helper()
	secret, err := s.create()
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestSessionSecretShape(t *testing.T) {
	s, _ := newTestSessions(t)
	a, b := mustCreate(t, s), mustCreate(t, s)
	if !lowerHex(a, 64) || !lowerHex(b, 64) {
		t.Fatalf("secrets are not 64 lower-case hex: %q %q", a, b)
	}
	if a == b {
		t.Fatal("two sessions share a secret")
	}
	for _, x := range []string{a, b} {
		raw, err := hex.DecodeString(x)
		if err != nil {
			t.Fatal(err)
		}
		forms := []string{x, string(raw), base64.StdEncoding.EncodeToString(raw), base64.URLEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw)}
		for _, dump := range []string{fmt.Sprintf("%#v", s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s.list)} {
			for _, f := range forms {
				if strings.Contains(dump, f) {
					t.Fatalf("the store holds a secret as %q", f)
				}
			}
		}
	}
}

func TestSessionValid(t *testing.T) {
	s, _ := newTestSessions(t)
	secret := mustCreate(t, s)
	if !s.valid(secret) {
		t.Fatal("the created secret is refused")
	}
	last := secret[len(secret)-1]
	other := byte('0')
	if last == '0' {
		other = '1'
	}
	if s.valid(secret[:len(secret)-1] + string(other)) {
		t.Fatal("a secret with its last character changed is accepted")
	}
	if s.valid("") {
		t.Fatal("the empty secret is accepted")
	}
}

func TestNinthSessionDropsTheOldest(t *testing.T) {
	s, c := newTestSessions(t)
	var secrets []string
	for i := 0; i < maxSessions+1; i++ {
		c.set(t0.Add(time.Duration(i) * time.Second))
		secrets = append(secrets, mustCreate(t, s))
	}
	if s.valid(secrets[0]) {
		t.Fatal("the oldest session survived a ninth")
	}
	for i, x := range secrets[1:] {
		if !s.valid(x) {
			t.Fatalf("session %d was dropped", i+1)
		}
	}
	if s.len() != maxSessions {
		t.Fatalf("len = %d, want %d", s.len(), maxSessions)
	}
}

// TestNinthDropsOldestByCreation: the oldest by creation goes, even when it
// was the most recently seen.
func TestNinthDropsOldestByCreation(t *testing.T) {
	s, c := newTestSessions(t)
	var secrets []string
	for i := 0; i < maxSessions; i++ {
		c.set(t0.Add(time.Duration(i) * time.Second))
		secrets = append(secrets, mustCreate(t, s))
	}
	c.set(t0.Add(time.Minute))
	if !s.valid(secrets[0]) {
		t.Fatal("the first session is refused")
	}
	ninth := mustCreate(t, s)
	if s.valid(secrets[0]) {
		t.Fatal("the least recently seen was dropped instead of the oldest by creation")
	}
	if !s.valid(secrets[1]) || !s.valid(ninth) {
		t.Fatal("a younger session was dropped")
	}
}

// TestExpiredDroppedBeforeTheCap: a ninth session evicts an expired one
// rather than a live session that was created earlier.
func TestExpiredDroppedBeforeTheCap(t *testing.T) {
	s, c := newTestSessions(t)
	a := mustCreate(t, s)
	c.set(t0.Add(time.Second))
	b := mustCreate(t, s)
	c.set(t0.Add(2 * time.Second))
	var others []string
	for i := 0; i < maxSessions-2; i++ {
		others = append(others, mustCreate(t, s))
	}
	c.set(t0.Add(time.Hour + 50*time.Minute))
	for _, x := range append([]string{a}, others...) {
		if !s.valid(x) {
			t.Fatal("a session expired early")
		}
	}
	c.set(t0.Add(2*time.Hour + time.Minute)) // b is idle for more than 2 h
	mustCreate(t, s)
	if !s.valid(a) {
		t.Fatal("a live session was dropped while an expired one was kept")
	}
	if s.valid(b) {
		t.Fatal("the expired session is still accepted")
	}
	if s.len() != maxSessions {
		t.Fatalf("len = %d, want %d", s.len(), maxSessions)
	}
}

// TestNoMonotonicReading: the store keeps wall-clock times only, so a
// suspend (which stops the monotonic clock) still counts as idle time.
func TestNoMonotonicReading(t *testing.T) {
	s := newSessions(time.Now)
	secret := mustCreate(t, s)
	if !s.valid(secret) {
		t.Fatal("the created secret is refused")
	}
	s.len()
	for _, x := range s.list {
		for _, tm := range []time.Time{x.created, x.lastSeen} {
			if strings.Contains(tm.String(), "m=") {
				t.Fatalf("a stored time carries a monotonic reading: %v", tm)
			}
		}
	}
}

func TestSessionIdle(t *testing.T) {
	s, c := newTestSessions(t)
	secret := mustCreate(t, s)
	c.set(t0.Add(2 * time.Hour))
	if !s.valid(secret) {
		t.Fatal("refused at exactly the idle limit")
	}
	c.set(t0.Add(4 * time.Hour))
	if !s.valid(secret) {
		t.Fatal("a use did not refresh the session")
	}
	c.set(t0.Add(6*time.Hour + time.Second))
	if s.valid(secret) {
		t.Fatal("accepted after more than the idle limit")
	}
	if s.len() != 0 {
		t.Fatalf("len = %d, the expired session was kept", s.len())
	}
}

func TestClockGoingBack(t *testing.T) {
	s, c := newTestSessions(t)
	secret := mustCreate(t, s)
	c.set(t0.Add(-3 * time.Hour))
	if !s.valid(secret) {
		t.Fatal("a clock stepped back expired the session")
	}
	back := t0.Add(2*time.Hour - time.Minute)
	c.set(back)
	if !s.valid(secret) {
		t.Fatal("lastSeen followed the clock back")
	}
	c.set(back.Add(2*time.Hour + time.Second))
	if s.valid(secret) {
		t.Fatal("a clock stepped back made the session immortal")
	}
}

func TestLookupComparesEverySession(t *testing.T) {
	s, _ := newTestSessions(t)
	first := mustCreate(t, s)
	for i := 1; i < maxSessions; i++ {
		mustCreate(t, s)
	}
	calls := 0
	old := compareSecret
	compareSecret = func(a, b []byte) int { calls++; return old(a, b) }
	defer func() { compareSecret = old }()
	if !s.valid(first) {
		t.Fatal("the first secret is refused")
	}
	if calls != maxSessions {
		t.Fatalf("compareSecret called %d times, want %d", calls, maxSessions)
	}
}

func TestSessionsAreGoroutineSafe(t *testing.T) {
	s, _ := newTestSessions(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				secret, err := s.create()
				if err != nil {
					t.Error(err)
					return
				}
				s.valid(secret)
				if n := s.len(); n > maxSessions {
					t.Errorf("len = %d", n)
				}
			}
		}()
	}
	wg.Wait()
	if s.len() > maxSessions {
		t.Fatalf("len = %d", s.len())
	}
}
