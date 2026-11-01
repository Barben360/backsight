package dashboard

import "net/http"

// StateView is what a state page shows: a heading, a sentence under it,
// and either a command to copy or a link that asks again. Kind is one of
// building, updating, updated, timeout, notfound, bad or broken.
type StateView struct {
	Kind, Title, Msg, Command, Retry string
}

// The state kinds.
const (
	kindBuilding = "building"
	kindUpdating = "updating"
	kindUpdated  = "updated"
	kindTimeout  = "timeout"
	kindNotFound = "notfound"
	kindBad      = "bad"
	kindBroken   = "broken"
)

// The state pages' fixed words. A page that answers a question (not found,
// bad request, broken) keeps the page's own question as its heading.
const (
	buildingTitle = "Preparing the dashboard."
	updatingTitle = "An update is being applied."
	updatingMsg   = "Try again in a minute."
	updatedTitle  = "backsight was updated."
	updatedMsg    = "Stop this dashboard (Ctrl+C in its terminal) and start it again:"
	timeoutTitle  = "The page took too long."
	timeoutMsg    = "Try again."
	restartCmd    = "backsight dashboard"
)

// stateOf is the view of a state page: title is the page's question and
// msg its sentence, both used by the kinds that have no words of their
// own; path is where Try again leads.
func stateOf(kind, title, msg, path string) *StateView {
	v := &StateView{Kind: kind, Title: title, Msg: msg}
	switch kind {
	case kindBuilding:
		v.Title = buildingTitle
	case kindUpdating:
		v.Title, v.Msg, v.Retry = updatingTitle, updatingMsg, path
	case kindUpdated:
		v.Title, v.Msg, v.Command = updatedTitle, updatedMsg, restartCmd
	case kindTimeout:
		v.Title, v.Msg, v.Retry = timeoutTitle, timeoutMsg, path
	}
	return v
}

// failureKind is the kind of the state page for a failure answered with
// status.
func failureKind(status int) string {
	switch status {
	case http.StatusNotFound:
		return kindNotFound
	case http.StatusBadRequest:
		return kindBad
	}
	return kindBroken
}
