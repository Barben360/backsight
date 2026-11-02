package cli

import (
	"context"
	"errors"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// The extraction hold (issue 149). The server accepts a session before its
// extraction LLM reads it: with a key the provider rejects, every session
// sent is accepted, then fails on the server, leaving no document, and the
// cursor has moved past it. So while the bank's latest finished retain
// failed and a dry-run extraction (server llm check's probe) fails on the
// provider, this PC sends nothing: sessions wait in the transcripts, notes
// in the queue. jobs tick decides and records it (state.Extraction); the
// other senders read the record.

const (
	// probeTimeout bounds the tick's dry-run extraction.
	probeTimeout = 90 * time.Second
	// probeQuiet: a probe that holds nothing is not made again for this
	// long while no hold is on (a passing probe costs a little extraction).
	// Held, every tick probes: a rejected key costs nothing.
	probeQuiet = time.Hour
)

// extractionHold is the recorded hold of bank's sends, "" when none.
func extractionHold(d paths.Dirs, bank string) string {
	if e := state.LoadExtraction(d); e.HeldFor(bank) {
		return e.Text()
	}
	return ""
}

// extractionGate decides the tick's hold and records it: "" sends, else
// why nothing goes out. The bank's latest finished retain is read first
// (two small reads): when it completed, or none failed, nothing is held.
// When it failed, the provider is probed with a dry-run extraction: a
// failure the server passes on from the provider (an HTTP 5xx; never the
// server refusing this PC's own key, 401 or 403) holds; a pass lifts the
// hold; anything else (no answer, a malformed or non-English fact) leaves
// the record as it was. A read that fails leaves it too.
func extractionGate(ctx context.Context, d paths.Dirs, cl *hindsight.Client, bank, provider string, now time.Time) string {
	e := state.LoadExtraction(d)
	held := e.HeldFor(bank)
	keep := func() string {
		if held {
			return e.Text()
		}
		return ""
	}
	failed, anyFailed, err := cl.LatestOperation(ctx, bank, "failed", "retain")
	if err != nil {
		return keep()
	}
	completed, anyCompleted, err := cl.LatestOperation(ctx, bank, "completed", "retain")
	if err != nil {
		return keep()
	}
	failing := anyFailed && (!anyCompleted || failed.FinishedAt().After(completed.FinishedAt()))
	if !failing {
		if held {
			_ = state.SaveExtraction(d, state.Extraction{Bank: bank, ProbeOKAt: e.ProbeOKAt, CheckedAt: e.CheckedAt})
		}
		return ""
	}
	if !held && (!e.ProbeOKAt.IsZero() && !failed.FinishedAt().After(e.ProbeOKAt) || now.Sub(e.CheckedAt) < probeQuiet) {
		return "" // that failure was before the provider last passed, or a probe ran lately
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	_, perr := server.CheckProvider(pctx, cl, bank, provider)
	cancel()
	var pe *server.ProviderError
	var ae *hindsight.APIError
	switch {
	case perr == nil:
		_ = state.SaveExtraction(d, state.Extraction{Bank: bank, ProbeOKAt: now, CheckedAt: now})
		return ""
	case errors.As(perr, &pe) && errors.As(perr, &ae) && ae.Status >= 500:
		if !held {
			e = state.Extraction{Bank: bank, HeldSince: now, ProbeOKAt: e.ProbeOKAt}
		}
		e.KeyRejected, e.CheckedAt = pe.KeyRejected, now
		_ = state.SaveExtraction(d, e)
		return e.Text()
	}
	if !held {
		e.Bank, e.CheckedAt = bank, now
		_ = state.SaveExtraction(d, e)
	}
	return keep()
}
