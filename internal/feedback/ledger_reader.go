package feedback

// LedgerReader is how anything but the folding reads the ledger. Today:
// CurrentReader, ledger.json plus the folded lines since its watermark,
// without the ledger lock. Block 7 swaps the implementation, nothing else.
type LedgerReader interface{ All() (Ledger, error) }

// CurrentReader reads Dir's ledger as Current does: it never takes the
// ledger lock, so a settle in progress never makes it wait.
type CurrentReader struct{ Dir string }

// All is Current(r.Dir).
func (r CurrentReader) All() (Ledger, error) { return Current(r.Dir) }
