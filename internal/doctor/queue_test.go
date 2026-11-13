package doctor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const opsPath = "/v1/default/banks/coding-agent%3A%3Amemoire/operations"

// queueServer answers the bank's queue with one retain created at queued
// (none when zero), and its latest completed retain as finished at done
// (none when zero); status, when non-zero, answers every queue read.
func queueServer(t *testing.T, queued, done time.Time, status int) *httptest.Server {
	t.Helper()
	base := fakeHandler(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != opsPath {
			base.ServeHTTP(w, r)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		switch r.URL.Query().Get("status") {
		case "pending":
			if !queued.IsZero() {
				_, _ = w.Write([]byte(`{"total":1,"operations":[{"id":"op1","operation_type":"retain","status":"pending","document_id":"conversation:s1#part3","created_at":"` +
					queued.UTC().Format(time.RFC3339) + `"}]}`))
				return
			}
		case "completed":
			if !done.IsZero() {
				_, _ = w.Write([]byte(`{"total":1,"operations":[{"id":"op0","operation_type":"retain","status":"completed","document_id":"conversation:s0","created_at":"` +
					done.Add(-time.Minute).UTC().Format(time.RFC3339) + `","updated_at":"` + done.UTC().Format(time.RFC3339) + `"}]}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"total":0,"operations":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func queueAgeAt(t *testing.T, srv *httptest.Server) Check {
	t.Helper()
	f := configured(t, srv.URL)
	f.env.Now = func() time.Time { return doctorAt }
	return check(t, f.run(), "queue-age")
}

// A retain waiting on the server for queueAgeLimit or more while no retain
// completed in the last hour: the queue does not move, which holds up the
// repair of a session's parts and the purge of a part landing late. A
// warning, in plain words, naming what to do.
func TestQueueAgeWarnsOnAQueueThatDoesNotMove(t *testing.T) {
	for name, done := range map[string]time.Time{"none completed": {}, "the last two hours ago": doctorAt.Add(-2 * time.Hour)} {
		t.Run(name, func(t *testing.T) {
			c := queueAgeAt(t, queueServer(t, doctorAt.Add(-7*time.Hour), done, 0))
			expect(t, c, Warn, "waited on the server for 7 h")
			if !strings.Contains(c.Detail, "purge") || !strings.Contains(c.Detail, "repair") || !strings.Contains(c.Fix, "backsight server llm check") {
				t.Errorf("%+v", c)
			}
		})
	}
}

// The same old retain while retains complete (a long import or backfill
// draining in order on a healthy server): information, never a fix that
// restarts a working server. A younger one, or none, is fine; a queue
// that cannot be read says so without a warning.
func TestQueueAgeOnAQueueStillDraining(t *testing.T) {
	c := queueAgeAt(t, queueServer(t, doctorAt.Add(-7*time.Hour), doctorAt.Add(-10*time.Minute), 0))
	expect(t, c, Info, "waited 7 h, still draining")
	if c.Fix != "" || strings.Contains(c.Detail, "server start") {
		t.Errorf("a fix for a draining queue: %+v", c)
	}
	expect(t, queueAgeAt(t, queueServer(t, doctorAt.Add(-time.Hour), time.Time{}, 0)), OK, "")
	expect(t, queueAgeAt(t, queueServer(t, time.Time{}, time.Time{}, 0)), OK, "")
	expect(t, queueAgeAt(t, queueServer(t, time.Time{}, time.Time{}, http.StatusServiceUnavailable)), Info, "cannot read")
}
