package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
)

// IndexError's fixed sentences, which update apply maps to its reasons.
const (
	UpdatingSentence   = "an update is being applied, try again in a minute"
	UnreadableSentence = "the metrics index is unreadable and rebuilding it failed today: run backsight stats index rebuild"
	ChangedSentence    = "backsight was updated meanwhile: run the command again"
)

// Usage is a usage error: the caller asked for something that cannot be.
func Usage(format string, args ...any) error {
	return &output.Error{Code: output.Usage, Message: fmt.Sprintf(format, args...)}
}

// ErrMalformedID marks the usage error of an id written wrongly in the
// request's path (a day that is not YYYY-MM-DD): the dashboard answers it
// as a page that does not exist, 404, where a bad query parameter is 400.
// The command line reports it as any usage error.
var ErrMalformedID = errors.New("malformed id")

// malformedID is a usage error that is an ErrMalformedID.
type malformedID struct{ usage *output.Error }

func (e malformedID) Error() string        { return e.usage.Error() }
func (e malformedID) Unwrap() error        { return e.usage }
func (e malformedID) Is(target error) bool { return target == ErrMalformedID }

// UnknownView is the usage error for a word that is not a view; withIndex
// adds "index", which only the command line has.
func UnknownView(word string, withIndex bool) error {
	names := slices.Clone(metrics.Views)
	if withIndex {
		names = append(names, "index")
	}
	return Usage("unknown view %q: one of %s", word, strings.Join(names, ", "))
}

// Error maps a metrics error to a reported one: an id that names nothing is
// a usage error, anything else an index error.
func Error(err error) error {
	var nf metrics.NotFoundError
	if !errors.As(err, &nf) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &output.Error{Code: output.Blocked, Message: "the view was interrupted or ran out of time"}
		}
		return IndexError(err)
	}
	switch {
	case nf.Kind == "metric":
		return Usage("no metric %q: backsight stats defs lists them", nf.ID)
	case nf.Kind == "project":
		return Usage("no project %q in the window", nf.ID)
	case nf.TooShort:
		return Usage("%s id: give at least 8 characters", nf.Kind)
	case len(nf.Matches) > 1:
		return Usage("ambiguous %s id %q: %s", nf.Kind, nf.ID, strings.Join(nf.Matches[:min(5, len(nf.Matches))], ", "))
	}
	return Usage("no %s %q in the metrics index", nf.Kind, nf.ID)
}

// IndexClassMessage is the sentence of an index error of class: a full
// disk and a busy index have their own; any other class names itself.
func IndexClassMessage(class string) string {
	switch class {
	case "full":
		return index.ErrDiskFull.Error()
	case "busy":
		return index.ErrInUse.Error()
	}
	return "the metrics index could not be read (" + class + "): run backsight stats index rebuild"
}

// IndexError is an index error as a command reports it: a sentence or a
// class, never the error's own text, which may hold a path.
func IndexError(err error) error {
	var oe *output.Error
	switch {
	case errors.As(err, &oe):
		return err
	case errors.Is(err, index.ErrUpdateInProgress):
		return &output.Error{Code: output.Blocked, Message: UpdatingSentence}
	case errors.Is(err, index.ErrInUse):
		return &output.Error{Code: output.Blocked, Message: index.ErrInUse.Error()}
	case errors.Is(err, index.ErrNeedsYes):
		return &output.Error{Code: output.NeedsUser, Message: "rebuilding drops the months listed, whose files are gone: confirm with backsight stats index rebuild --yes",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight stats index rebuild --yes", Why: "rebuild without those months"}}}
	case errors.Is(err, index.ErrUnreadable):
		return &output.Error{Code: output.Blocked, Message: UnreadableSentence}
	case errors.Is(err, index.ErrSchemaChanged):
		return &output.Error{Code: output.Blocked, Message: ChangedSentence}
	}
	return &output.Error{Code: output.Blocked, Message: IndexClassMessage(index.Class(err))}
}
