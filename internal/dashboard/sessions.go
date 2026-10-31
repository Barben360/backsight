package dashboard

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	// sessionIdle is how long a session lives without being seen.
	sessionIdle = 2 * time.Hour
	// maxSessions is how many sessions are live at once: a ninth drops
	// the oldest.
	maxSessions = 8
)

// compareSecret compares two hashes in constant time: tests count its calls.
var compareSecret = subtle.ConstantTimeCompare

// session is one signed-in browser. Only the SHA-256 of its secret is kept.
type session struct {
	hash     [32]byte
	created  time.Time
	lastSeen time.Time
}

// sessions is the dashboard's in-memory session store.
type sessions struct {
	mu   sync.Mutex
	list []session // oldest created first
	now  func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{now: now}
}

// create starts a session: 256 random bits as 64 lower-case hex characters,
// returned once and never stored. With maxSessions live, the oldest by
// creation is dropped.
func (s *sessions) create() (string, error) {
	var b [32]byte
	if _, err := randRead(b[:]); err != nil {
		return "", fmt.Errorf("draw a session secret: %w", err)
	}
	secret := hex.EncodeToString(b[:])
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(now)
	if len(s.list) >= maxSessions {
		s.list = s.list[len(s.list)-maxSessions+1:]
	}
	s.list = append(s.list, session{hash: sha256.Sum256([]byte(secret)), created: now, lastSeen: now})
	return secret, nil
}

// valid: secret belongs to a live session. Its hash is compared with every
// live hash, with no early exit, so timing does not tell which matched.
// Expired sessions are dropped; the match's lastSeen moves forward only, so
// a clock stepped back neither expires a session nor keeps it alive.
func (s *sessions) valid(secret string) bool {
	h := sha256.Sum256([]byte(secret))
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(now)
	found := -1
	for i := range s.list {
		if compareSecret(h[:], s.list[i].hash[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return false
	}
	if now.After(s.list[found].lastSeen) {
		s.list[found].lastSeen = now
	}
	return true
}

// len is the number of live sessions.
func (s *sessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpired(s.now())
	return len(s.list)
}

// dropExpired removes the sessions idle for more than sessionIdle. The
// caller holds mu.
func (s *sessions) dropExpired(now time.Time) {
	kept := s.list[:0]
	for _, x := range s.list {
		if now.Sub(x.lastSeen) <= sessionIdle {
			kept = append(kept, x)
		}
	}
	s.list = kept
}
