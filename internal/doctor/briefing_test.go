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
	if !strings.Contains(c.Detail, "1 briefing page(s) have another refresh trigger than this PC's (floor 6h): "+drifted+" (found no floor)") || c.Fix != "backsight jobs maintain" {
		t.Errorf("drifted: %+v", c)
	}

	// Another floor here (another PC set 6 h): the page says the floor found.
	c = check(t, briefingFixture(t, model(ok, floor), "[recall]\nbriefing_refresh_interval = \"1h\"\n").run(), "bank")
	expect(t, c, Warn, "bank coding-agent::memoire")
	if !strings.Contains(c.Detail, "than this PC's (floor 1h): "+ok+" (found floor 6h)") {
		t.Errorf("configured floor: %+v", c)
	}

	// No floor wanted: a null floor is no floor, an explicit 6 h is not.
	c = check(t, briefingFixture(t, model(drifted, none)+","+model(ok, floor), "[recall]\nbriefing_refresh_interval = \"0\"\n").run(), "bank")
	if !strings.Contains(c.Detail, "1 briefing page(s) have another refresh trigger than this PC's (no floor): "+ok+" (found floor 6h)") || strings.Contains(c.Detail, drifted) {
		t.Errorf("floor 0: %+v", c)
	}

	// Same floor, another setting.
	c = check(t, briefingFixture(t, model(ok, strings.Replace(floor, `"budget":"low"`, `"budget":"mid"`, 1)), "").run(), "bank")
	if !strings.Contains(c.Detail, ok+` (found budget "mid", mode "full", refresh after consolidation true)`) {
		t.Errorf("budget: %+v", c)
	}

	// briefing = "recent": the pages go at the next tick, nothing to say.
	c = check(t, briefingFixture(t, model(drifted, none), "[recall]\nbriefing = \"recent\"\n").run(), "bank")
	expect(t, c, OK, "bank coding-agent::memoire")
}
