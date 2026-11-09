package telemetry

// The reasons a feedback write failed. A closed set: the event never
// carries an error text.
const (
	FeedbackReasonJournalRead   = "journal_read"
	FeedbackReasonJournalWrite  = "journal_write"
	FeedbackReasonTurnPointer   = "turn_pointer"
	FeedbackReasonSettle        = "settle" // kept for older events: no longer emitted (a tick's settle fails as ledger_fold)
	FeedbackReasonSharedCombine = "shared_combine"
	// The ledger database: a reader that fell back to ledger.json
	// (missing, busy, unreadable, of another schema), a ledger.json that
	// could not be read either, an export of ledger.json that failed, a
	// tick's settle or fold that failed.
	FeedbackReasonLedgerMissing    = "ledger_missing"
	FeedbackReasonLedgerBusy       = "ledger_busy"
	FeedbackReasonLedgerUnreadable = "ledger_unreadable"
	FeedbackReasonLedgerSchema     = "ledger_schema"
	FeedbackReasonLedgerJSON       = "ledger_json"
	FeedbackReasonLedgerExport     = "ledger_export"
	FeedbackReasonLedgerFold       = "ledger_fold"
)

// FeedbackError is the feedback_error event for reason, one of the
// FeedbackReason* constants.
func FeedbackError(reason string) Raw {
	return Raw{Name: "feedback_error", Fields: map[string]any{"reason": reason}}
}
