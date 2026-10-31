package telemetry

// The reasons a feedback write failed. A closed set: the event never
// carries an error text.
const (
	FeedbackReasonJournalRead   = "journal_read"
	FeedbackReasonJournalWrite  = "journal_write"
	FeedbackReasonTurnPointer   = "turn_pointer"
	FeedbackReasonSettle        = "settle"
	FeedbackReasonSharedCombine = "shared_combine"
)

// FeedbackError is the feedback_error event for reason, one of the
// FeedbackReason* constants.
func FeedbackError(reason string) Raw {
	return Raw{Name: "feedback_error", Fields: map[string]any{"reason": reason}}
}
