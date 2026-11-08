package doctor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/testtool"
)

const modelsPath = "/v1/default/banks/coding-agent%3A%3Amemoire/mental-models"

// briefingFixture is a configured install whose server lists models.
func briefingFixture(t *testing.T, models string, extra string) *fixture {
	t.Helper()
	srv := fakeServerWith(t, map[string]reply{modelsPath: {http.StatusOK, `{"items":[` + models + `]}`}})
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = "+testtool.TOMLStr(srv.URL)+"\nbank = \"coding-agent::memoire\"\n"+extra, 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	return f
}

func model(id, trigger string) string { return `{"id":"` + id + `","trigger":` + trigger + `}` }

// Decision 0083 C2: the bank check reports a briefing page that refreshes
// without the configured floor, and the job that sets it.
func TestBankReportsADriftedBriefingTrigger(t *testing.T) {
	floor := `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":21600}`
	none := `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":null}`
	ok, drifted := retain.BriefingModelID("billing"), retain.BriefingModelID("ops")

	c := check(t, briefingFixture(t, model(ok, floor)+","+model("team-notes", none), "").run(), "bank")
	expect(t, c, OK, "bank coding-agent::memoire")
	if strings.Contains(c.Detail, "briefing") {
		t.Errorf("every page right (another mental model is not backsight's): %+v", c)
	}

	c = check(t, briefingFixture(t, model(ok, floor)+","+model(drifted, none), "").run(), "bank")
	expect(t, c, Warn, "bank coding-agent::memoire")
	if !strings.Contains(c.Detail, "1 of 2 briefing page(s) refresh without the 6h floor ("+drifted+")") || c.Fix != "backsight jobs maintain" {
		t.Errorf("drifted: %+v", c)
	}

	// Another floor configured: the page with 6 h is the drifted one.
	c = check(t, briefingFixture(t, model(ok, floor), "[recall]\nbriefing_refresh_interval = \"1h\"\n").run(), "bank")
	expect(t, c, Warn, "bank coding-agent::memoire")
	if !strings.Contains(c.Detail, "without the 1h floor ("+ok+")") {
		t.Errorf("configured floor: %+v", c)
	}

	// briefing = "recent": the pages go at the next tick, nothing to say.
	c = check(t, briefingFixture(t, model(drifted, none), "[recall]\nbriefing = \"recent\"\n").run(), "bank")
	expect(t, c, OK, "bank coding-agent::memoire")
}
