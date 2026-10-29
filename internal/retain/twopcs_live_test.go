package retain_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/livecheck"
	"github.com/Barben360/backsight/internal/retain"
)

// consolidated is the server both PCs sent to, whose documents each gave
// one consolidated fact and an observation from it (the fake server
// extracts none).
type consolidated struct{ *hindsight.Client }

func (s consolidated) ListMemories(_ context.Context, _ string, q hindsight.MemoryQuery, fn func(hindsight.Memory) error) error {
	if q.Type == "observation" {
		return fn(hindsight.Memory{ID: "o1", FactType: "observation", SourceMemoryIDs: []string{"f-conversation:sp1"}})
	}
	return fn(hindsight.Memory{ID: "f-" + q.DocumentID, FactType: "world", DocumentID: q.DocumentID, ConsolidatedAt: "2026-10-24T12:30:00+00:00"})
}

func (consolidated) QueuedOperations(context.Context, string) (int, error) { return 0, nil }

// After B numbered its segment after A's (segment 3), A's verify --live
// passes and names the document B continued: B's segment is a later one,
// from another machine id, not a loss of A's.
func TestLiveCheckPassesOnThePCWhoseSegmentAnotherPCFollowed(t *testing.T) {
	pc := retain.ContinuedElsewhere(t)
	written := pc.SentAt.Add(-time.Hour)
	if err := os.Chtimes(pc.Transcript, written, written); err != nil {
		t.Fatal(err)
	}
	r := livecheck.Check(context.Background(), livecheck.Input{Server: consolidated{pc.Server}, Bank: pc.Bank, Transcripts: []string{pc.Transcript},
		CursorsDir: pc.Cursor, Now: pc.SentAt.Add(2 * time.Hour), MachineID: pc.MachineID})
	if r.Status != livecheck.Pass || !strings.Contains(r.Summary, "continued on another PC: conversation:sp1") {
		t.Fatalf("%s %q (detail %q)", r.Status, r.Summary, r.Detail)
	}
	if len(r.ContinuedElsewhere) != 1 || r.ContinuedElsewhere[0] != "conversation:sp1" || r.Segment != 2 || r.ServerSegment != 3 {
		t.Errorf("%+v", r)
	}
}
