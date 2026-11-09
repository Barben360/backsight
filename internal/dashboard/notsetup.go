package dashboard

import "github.com/Barben360/backsight/internal/metrics"

// noServerSentence is what a widget says on a PC with no memory server set
// up, where a server that does not answer would say "not answering".
const noServerSentence = "No memory server is set up on this PC."

// noServerSetUp says whether r was computed on a PC with no memory server
// set up: its server read failed as not_configured (the enum has the
// reason's word), or, for a view that reads nothing from the server, its
// scope names none.
func noServerSetUp(r *metrics.Result) bool {
	if r.Server != nil {
		return !r.Server.OK && r.Server.Error == metrics.ReasonNotConfigured
	}
	return r.Scope.Server == ""
}

// frameNoServer is noServerSetUp for a page's frame: its first view's
// server read, else its scope.
func frameNoServer(f Frame) bool {
	if f.ServerRead != nil {
		return !f.ServerRead.OK && f.ServerRead.Error == metrics.ReasonNotConfigured
	}
	return f.Scope != nil && f.Scope.Server == ""
}

// serverAbsent is why the server's figures are missing, as a clause: no
// server set up, or one that is not answering.
func serverAbsent(r *metrics.Result) string {
	if noServerSetUp(r) {
		return "no memory server set up"
	}
	return "the memory server is not answering"
}

// memoryOn is the copied summary's word for where the memory lives:
// "memory on" its host, or "no memory server set up".
func memoryOn(server string) string {
	if server == "" {
		return "no memory server set up"
	}
	return "memory on " + memoryHost(server)
}
