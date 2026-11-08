package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The briefing pages' trigger (decision 0083 C2) and request (C7): every
// page of the bank is given the wanted ones, so that pages made by an
// earlier version get the refresh floor and the current request too. jobs
// maintain does it daily; jobs tick once per binary version, floor and
// request (its marker), so an update applies it within a tick.

// briefingQuery is the request the pages are written from (a variable for
// the tests).
var briefingQuery = instructions.BriefingQuery

// briefingTriggerRetry is how long the ticks wait before trying again
// after a failure (a page the server refuses, a listing that failed):
// a page that keeps failing costs a listing an hour, not one per tick.
const briefingTriggerRetry = time.Hour

// briefingMarker is the last reconcile: the binary version, floor and
// request it ran for, and the pages that failed (with when), "" none.
type briefingMarker struct {
	Key      string    `json:"key"`
	Failed   []string  `json:"failed,omitempty"`
	FailedAt time.Time `json:"failed_at,omitzero"`
}

func briefingTriggerKey(cfg config.Config) string {
	q := sha256.Sum256([]byte(briefingQuery))
	return buildinfo.Version + " " + strconv.Itoa(cfg.Recall.BriefingRefreshInterval.Seconds()) + " " + hex.EncodeToString(q[:6])
}

func writeBriefingMarker(d paths.Dirs, m briefingMarker) {
	if raw, err := json.Marshal(m); err == nil {
		_ = fsutil.WriteAtomic(d.BriefingTriggerFile(), append(raw, '\n'), 0o600)
	}
}

// reconcileBriefingTriggers sets the wanted trigger and request on every
// briefing page where either differs, emits what it did, and records the marker (with
// the failures, if any). Its line is "" when every page was already right.
func reconcileBriefingTriggers(ctx context.Context, d paths.Dirs, cfg config.Config, cl *hindsight.Client, tw *telemetry.Writer, job string) string {
	key := briefingTriggerKey(cfg)
	r, err := retain.ReconcileBriefingTriggers(ctx, cl, cfg.Server.Bank, retain.BriefingTrigger(cfg.Recall.BriefingRefreshInterval.Seconds()), briefingQuery)
	if err != nil {
		tw.Emit(telemetry.Raw{Name: "briefing_trigger", Fields: map[string]any{"job": job, "error": err.Error()}})
		writeBriefingMarker(d, briefingMarker{Key: key, Failed: []string{"listing: " + err.Error()}, FailedAt: now().UTC()})
		return "briefing pages not checked: " + err.Error()
	}
	if r.Checked > 0 { // a bank without pages has nothing to count
		tw.Emit(telemetry.Raw{Name: "briefing_trigger", Fields: map[string]any{"job": job, "checked": r.Checked, "patched": len(r.Patched), "failed": len(r.Failed), "gone": len(r.Gone)}})
	}
	m := briefingMarker{Key: key}
	if len(r.Failed) > 0 {
		m.Failed, m.FailedAt = r.Failed, now().UTC()
	}
	writeBriefingMarker(d, m)
	var line string
	if len(r.Patched) > 0 {
		line = fmt.Sprintf("%d briefing page(s) given the %s refresh floor and the current briefing request", len(r.Patched), cfg.Recall.BriefingRefreshInterval)
	}
	if len(r.Failed) > 0 {
		line = strings.TrimPrefix(line+fmt.Sprintf("; %d briefing page(s) not updated (%s), tried again in an hour", len(r.Failed), strings.Join(r.Failed, "; ")), "; ")
	}
	return line
}

// tickBriefingTriggers reconciles once per binary version, floor and
// request, and again at most hourly while a page failed; "" when it does
// not run. The caller has checked the server is the joined one.
func tickBriefingTriggers(ctx context.Context, d paths.Dirs, cfg config.Config, cl *hindsight.Client, tw *telemetry.Writer) string {
	if briefingFor(cfg) != "model" {
		return ""
	}
	var m briefingMarker
	if raw, err := os.ReadFile(d.BriefingTriggerFile()); err == nil && json.Unmarshal(raw, &m) == nil && m.Key == briefingTriggerKey(cfg) &&
		(len(m.Failed) == 0 || now().Sub(m.FailedAt) < briefingTriggerRetry) {
		return ""
	}
	return reconcileBriefingTriggers(ctx, d, cfg, cl, tw, "tick")
}

// maintainBriefingTriggers is jobs maintain's daily reconcile; "" when
// there is nothing to do or say.
func maintainBriefingTriggers(ctx context.Context, d paths.Dirs, tw *telemetry.Writer) string {
	cfg, sec, err := loadServerConfig()
	if err != nil || briefingFor(cfg) != "model" || otherServer(d, cfg) != nil {
		return ""
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 10*time.Second)
	if err != nil {
		return ""
	}
	return reconcileBriefingTriggers(ctx, d, cfg, cl, tw, "maintain")
}
