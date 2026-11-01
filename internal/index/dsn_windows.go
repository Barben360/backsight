//go:build windows

package index

// writerPragmas are the writer's own pragmas on Windows. SQLite
// checkpoints once the WAL holds 1000 pages; a first build's batch dirties
// thousands (the memory and session indexes' leaves), so every batch
// checkpointed, and each checkpoint flushed its scattered pages to the
// file with FlushFileBuffers: 33 s of the VM's 71 s first build, and over
// 10 min on windows-latest, whose disk flushes about 340 random pages a
// second (Task 12b). Checkpointing at 256 MiB of WAL writes a page that
// several batches rewrote once (VM: 32 s alone, 42 s beside the other
// packages' tests; 64 MiB: 39 s and 53 s); a 64 MiB page cache keeps a
// batch's dirty pages instead of spilling them to the WAL mid-transaction
// and writing them again at its commit. Crash safety is unchanged
// (synchronous stays NORMAL: a commit is synced by the next checkpoint). A
// power cut can roll back the batches committed since the last checkpoint,
// now up to 256 MiB of WAL instead of about one batch; the next catch-up
// reads them again, their cursors rolled back with them. The WAL peaks
// near 256 MiB during a long catch-up, and journal_size_limit cuts it
// back to 64 MiB once a checkpoint resets it.
const writerPragmas = "&_pragma=wal_autocheckpoint(65536)&_pragma=cache_size(-65536)"
