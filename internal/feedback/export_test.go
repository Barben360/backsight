package feedback

// OnLockWait sets what runs each time a lock wait finds the ledger lock
// held, and returns the call that clears it.
func OnLockWait(f func()) (clear func()) {
	inLockWait = f
	return func() { inLockWait = nil }
}
