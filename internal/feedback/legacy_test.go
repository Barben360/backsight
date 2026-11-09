package feedback

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/fsutil"
)

// afterSettleRead, when set (tests), runs once LegacySettle has read the journal.
var afterSettleRead func()

// LegacySettle is v0.1.32's settle, the one the tick ran before the
// ledger database, kept unchanged as the rollback proof: an older binary
// put back by a rollback runs it over the ledger.json the database
// exported.
//
// It decides the shows of dir's journal that can settle at now, the
// clock read once it holds the ledger lock, journals them as one settle
// event carrying p, and brings ledger.json up to date: the settle,
// forget, retire, hide and unhide events since its watermark, in time
// order, each settle key once. Under the ledger lock: a concurrent settle
// gets a *state.HeldError and changes nothing. A ledger of another schema
// is left alone, and nothing is journaled; one that does not parse is
// kept aside and rebuilt from the journal first (rebuildCorrupt).
func LegacySettle(dir string, p Params, clock func() time.Time) (SettleResult, error) {
	var res SettleResult
	release, err := lockLedger(dir)
	if err != nil {
		return res, err
	}
	defer release()
	now := clock()
	l, err := LoadLedger(dir)
	if errors.Is(err, ErrCorruptLedger) {
		l, res.Rebuilt, err = rebuildCorrupt(dir, p, now)
	}
	if err != nil {
		return res, err
	}
	at := journalEnd(dir)
	events, _, err := Read(dir, now.Add(-SettleWindow))
	if err != nil {
		return res, err
	}
	if afterSettleRead != nil {
		afterSettleRead()
	}
	var apply []Event
	applied := map[string]bool{} // the keys of the settle events the ledger holds
	for _, e := range events {
		switch {
		case e.Kind == "settle" && !e.TS.After(l.Watermark):
			for _, s := range e.Settled {
				applied[s.Key] = true
			}
		case (e.Kind == "settle" || Folds(e.Kind)) && e.TS.After(l.Watermark) && !e.TS.After(now):
			apply = append(apply, e) // a ledger write that failed, and forgets and retirements since the last settle
		}
	}
	settled, late := Decide(events, p, now)
	if len(settled) > 0 || late > 0 {
		// Even with no show, the event marks where these late credits were
		// counted: the next settle counts only later ones.
		e := Event{Kind: "settle", TS: now.UTC(), Settled: settled, Params: p.Record()}
		j := NewJournal(dir, Common{Binary: buildinfo.Version})
		j.now = func() time.Time { return now }
		if err := j.Append(e); err != nil {
			return res, err
		}
		apply = append(apply, e)
	}
	for _, e := range SortByTime(apply) {
		if e.Kind == "settle" {
			e = Unapplied(e, seenOnce(applied)) // a settle run while its lock was wrongly broken
		}
		l.Apply(e, p)
	}
	l.Watermark = now.UTC()
	// Current reads on from where this read began, unless a folded event
	// before it is newer than the watermark: left for Current to apply,
	// it needs the read from the watermark. So does a journal that already
	// ends in a later month than now's: a folded event stamped just after
	// the watermark goes to now's month, before that end.
	l.JournalAt = at
	if at.File > MonthFile(now) ||
		slices.ContainsFunc(events, func(e Event) bool { return Folds(e.Kind) && e.TS.After(now) }) {
		l.JournalAt = JournalPos{}
	}
	if err := saveLedger(dir, l); err != nil {
		return res, err
	}
	res.Late = late
	for _, s := range settled {
		if s.Outcome == OutcomeForgotten {
			continue
		}
		res.Shows++
		switch s.Outcome {
		case OutcomeCredit:
			res.Credits++
		case OutcomeMiss:
			res.Misses++
		case OutcomeUnknown:
			res.Unknown++
		}
	}
	return res, nil
}

// rebuildCorrupt rebuilds dir's ledger from the journal, as feedback
// rebuild does, copies the ledger.json that did not parse to
// ledger.json.corrupt-<now in unix s> (a -2, -3… suffix when an earlier
// copy has that name: none is ever replaced), and writes the rebuilt one
// over it atomically; it returns the ledger and the copy's name. For a
// caller holding the ledger lock. A journal it cannot read, or a copy it
// cannot make, leaves the file as it was, for the next settle to try
// again.
func rebuildCorrupt(dir string, p Params, now time.Time) (Ledger, string, error) {
	l, err := rebuild(dir, p, true)
	if err != nil {
		return l, "", err
	}
	raw, err := os.ReadFile(ledgerFile(dir))
	if err != nil {
		return l, "", err
	}
	aside, err := keepAside(ledgerFile(dir)+".corrupt-"+strconv.FormatInt(now.Unix(), 10), raw)
	if err != nil {
		return l, "", err
	}
	return l, filepath.Base(aside), saveLedger(dir, l)
}

// keepAside writes raw to a new file named base, or base-2, base-3… when
// that name is taken, and returns its path.
func keepAside(base string, raw []byte) (string, error) {
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(raw)
		if serr := f.Sync(); werr == nil {
			werr = serr
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(name)
		}
		return name, werr
	}
}

// journalEnd is where dir's journal ends: its last month file and that
// file's size; zero without a journal. Taken under the ledger lock before
// a read, so that no folded event lands between it and the read.
func journalEnd(dir string) JournalPos {
	files, err := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	if err != nil || len(files) == 0 {
		return JournalPos{}
	}
	slices.Sort(files)
	last := files[len(files)-1]
	fi, err := os.Stat(last)
	if err != nil {
		return JournalPos{}
	}
	return JournalPos{File: filepath.Base(last), Offset: fi.Size()}
}

func saveLedger(dir string, l Ledger) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(ledgerFile(dir), raw, 0o600)
}

// Rebuild folds every journal file of dir, under the ledger lock, and
// writes the result only when write; without withImport, it leaves the
// import events out (the ledger without earlier history). Each settle
// or import event folds with the half-lives it recorded, so a rebuild
// after a config change gives the live ledger back; p's half-lives serve
// only events that recorded none. Recomputing under other parameters is
// not a rebuild.
func Rebuild(dir string, p Params, write, withImport bool) (Ledger, error) {
	release, err := lockLedger(dir)
	if err != nil {
		return Ledger{}, err
	}
	defer release()
	l, err := rebuild(dir, p, withImport)
	if err != nil {
		return Ledger{}, err
	}
	if write {
		if err := saveLedger(dir, l); err != nil {
			return l, err
		}
	}
	return l, nil
}

// rebuild is Rebuild's fold, written nowhere, for a caller holding the
// ledger lock.
func rebuild(dir string, p Params, withImport bool) (Ledger, error) {
	at := journalEnd(dir)
	events, _, err := Read(dir, time.Time{})
	if err != nil {
		return Ledger{}, err
	}
	kept := events
	if !withImport {
		kept = DropImports(events)
	}
	l := Fold(kept, p)
	l.JournalAt = at // Fold applied every folded event before it
	// Nor does the next settle apply them again (an import left out
	// included): it applies what is newer than the watermark.
	for _, e := range events {
		if Folds(e.Kind) {
			l.Watermark = laterOf(l.Watermark, e.TS)
		}
	}
	return l, nil
}
