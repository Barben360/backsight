package retain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/transcript"
)

// CountOrphanNotes counts the saved notes (note: documents) of bank, and
// among them those a backfill of transcripts would not send again: neither
// the replay of one of their saves nor a note stored live in one of their
// sessions. A PC leaving its own server keeps those in its old bank only.
func CountOrphanNotes(ctx context.Context, cl *hindsight.Client, bank string, transcripts []string) (orphans, total int, err error) {
	docs, err := cl.ListDocuments(ctx, bank)
	if err != nil {
		return 0, 0, errors.New(errSummary(err))
	}
	sessions, replays := map[string]bool{}, map[string]bool{}
	for _, p := range transcripts {
		fh, err := os.Open(p)
		if err != nil {
			continue // backfill skips it too
		}
		recs, _, _ := transcript.Read(fh, 0)
		_ = fh.Close()
		sid := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		sessions[sid] = true
		// The replay id does not depend on the project: every save counts.
		for _, n := range NotesFromRecords(bank, sid, recs, nil, func(transcript.Record) (string, bool) { return "", true }) {
			replays[n.DocumentID] = true
		}
	}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, "note:") {
			continue
		}
		total++
		if !replays[d.ID] && !sessions[d.Metadata["session"]] {
			orphans++
		}
	}
	return orphans, total, nil
}
