package doctor

import (
	"strings"
	"testing"
	"time"
)

const opsPath = "/v1/default/banks/coding-agent%3A%3Amemoire/operations"

// queuedRetain is the server's queue holding one retain created at.
func queuedRetain(at time.Time) reply {
	return reply{200, `{"total":1,"operations":[{"id":"op1","operation_type":"retain","status":"pending","document_id":"conversation:s1#part3","created_at":"` +
		at.UTC().Format(time.RFC3339) + `"}]}`}
}

// A retain waiting on the server for queueAgeLimit or more holds up the
// repair of a session's parts and the purge of a part landing late: a
// warning, in plain words, naming what to do. A younger one, or none, is
// fine; a queue that cannot be read says so without a warning.
func TestQueueAgeWarnsOnARetainWaitingForHours(t *testing.T) {
	at := func(f *fixture) *fixture { f.env.Now = func() time.Time { return doctorAt }; return f }
	f := at(configured(t, fakeServerWith(t, map[string]reply{opsPath: queuedRetain(doctorAt.Add(-7 * time.Hour))}).URL))
	c := check(t, f.run(), "queue-age")
	expect(t, c, Warn, "waited on the server for 7 h")
	if !strings.Contains(c.Detail, "purge") || !strings.Contains(c.Detail, "repair") || !strings.Contains(c.Fix, "backsight server llm check") {
		t.Errorf("%+v", c)
	}
	f = at(configured(t, fakeServerWith(t, map[string]reply{opsPath: queuedRetain(doctorAt.Add(-time.Hour))}).URL))
	expect(t, check(t, f.run(), "queue-age"), OK, "")
	f = configured(t, fakeServerWith(t, map[string]reply{opsPath: {200, `{"total":0,"operations":[]}`}}).URL)
	expect(t, check(t, f.run(), "queue-age"), OK, "")
	f = configured(t, fakeServerWith(t, map[string]reply{opsPath: {503, `{}`}}).URL)
	expect(t, check(t, f.run(), "queue-age"), Info, "cannot read")
}
