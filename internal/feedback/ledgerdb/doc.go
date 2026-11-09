// Package ledgerdb owns the feedback ledger's database,
// <state>/feedback/ledger.db: a SQLite file derived from the journal
// month files, which stay the source of truth (any ledger.db can be
// rebuilt from them). It holds its migration registry; the writer, which
// creates and migrates the file, uses a side file beside one a newer
// binary migrated, and sets a corrupt one aside; and the read-only
// reader, which never creates, migrates or writes it. It imports
// internal/feedback, never the reverse, and never the metrics index.
// Hooks reach it only through the reader.
package ledgerdb
