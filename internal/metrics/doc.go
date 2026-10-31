// Package metrics is the single read model behind the memory dashboard:
// the views that `backsight stats <view> --json` prints and that the
// dashboard's HTTP API and pages serve. It reads the local metrics index
// (internal/index) and the server's read-only endpoints; it imports no HTTP
// server code and no command-line code, and no free text (prompts, errors,
// notes, memory text) leaves the index into a view.
package metrics
