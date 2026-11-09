package metrics

import (
	"testing"

	"github.com/Barben360/backsight/internal/telemetry"
)

// TestDaysCountDocumentsSent: a day's Sent and Failed count documents, each
// once a day at its last send of the day, not send events. On the 28th a
// session sent twice (sent in parts) counts once; a project document that
// failed then went out counts sent; a note sent then refused counts failed.
// Sent again on the 29th, the session counts on the 29th too. A day with
// no send reads zeros.
func TestDaysCountDocumentsSent(t *testing.T) {
	x := newFx(t, fxNow)
	send := func(day, hour int, doc, status string) {
		x.tel(paris(day, hour, 0), telemetry.Retain{Document: doc, Items: 1, Status: status, MS: 5}, "s1", "alpha")
	}
	send(28, 10, "conversation:s1", "ok")
	send(28, 11, "conversation:s1", "ok")
	send(28, 12, "conversation:s1@beta", "failed")
	send(28, 13, "conversation:s1@beta", "ok")
	send(28, 14, "note:n1", "ok")
	send(28, 15, "note:n1", "failed")
	send(28, 16, "note:n2", "retry") // not a send that ended
	send(29, 9, "conversation:s1", "ok")
	w, err := NewWindow(fxNow, 0, paris(28, 0, 0), paris(31, 0, 0), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	rows := x.run("days", Query{Window: w, Machines: "this"}).Tables.Days
	got := map[string][2]int64{}
	for _, r := range rows {
		got[r.Day] = [2]int64{r.SentOK, r.SentFailed}
	}
	for day, want := range map[string][2]int64{"2026-10-28": {2, 1}, "2026-10-29": {1, 0}, "2026-10-30": {0, 0}} {
		if got[day] != want {
			t.Errorf("%s: sent %d, failed %d; want %d, %d", day, got[day][0], got[day][1], want[0], want[1])
		}
	}
}
