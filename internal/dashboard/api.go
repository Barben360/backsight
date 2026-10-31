package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/views"
)

// The API's fixed sentences: a state, a deadline, a failure whose own
// text is never shown.
const (
	buildingSentence  = "the metrics index is being built: try again in a few seconds"
	updatedSentence   = "backsight was updated: restart the dashboard (Ctrl+C, then backsight dashboard)"
	tooSlowSentence   = "the page took too long: try again"
	viewFailsSentence = "the view failed: run backsight doctor"
	noPageSentence    = "no such page"
)

// serveAPI answers /api/v1/{view}[/{id}]: the JSON that `backsight stats
// <view> [<id>] --json` prints for the same parameters.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	view, id := r.PathValue("view"), r.PathValue("id")
	command := "stats " + view
	if !isView(view) {
		apiError(w, command, http.StatusNotFound, views.UnknownView(view, false))
		return
	}
	if id != "" && !views.TakesID(view) {
		apiError(w, command, http.StatusNotFound, views.Usage(noPageSentence))
		return
	}
	p, err := views.FromValues(view, r.URL.Query())
	if err != nil {
		apiError(w, command, http.StatusBadRequest, err)
		return
	}
	res, st, err := s.data.result(r.Context(), view, p, id)
	if st == stateReady && errors.Is(err, index.ErrUpdateInProgress) {
		st, err = stateUpdating, nil
	}
	switch st {
	case stateBuilding:
		w.Header().Set("Retry-After", "2")
		apiError(w, command, http.StatusServiceUnavailable, &output.Error{Code: output.Blocked, Message: buildingSentence})
		return
	case stateUpdating:
		apiError(w, command, http.StatusServiceUnavailable, &output.Error{Code: output.Blocked, Message: views.UpdatingSentence})
		return
	case stateUpdated:
		apiError(w, command, http.StatusServiceUnavailable, &output.Error{Code: output.Blocked, Message: updatedSentence})
		return
	}
	if st == stateBroken && err == nil {
		err = errNoIndex
	}
	if err != nil {
		if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
			err = context.DeadlineExceeded // a broken state met on the way is the same slowness
		}
		status, reported := apiFailure(err)
		apiError(w, command, status, reported)
		return
	}
	var buf bytes.Buffer
	if err := output.WriteJSON(&buf, command, res, nil); err != nil {
		apiError(w, command, http.StatusInternalServerError, &output.Error{Code: output.Blocked, Message: viewFailsSentence})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// isView reports that name is a view the API serves.
func isView(name string) bool {
	for _, v := range metrics.Views {
		if v == name {
			return true
		}
	}
	return false
}

// httpStatus is the HTTP status of a view's raw error: an id or view that
// names nothing is 404, a deadline or a blocked environment 503, a usage
// error 400, anything else 500.
func httpStatus(err error) int {
	var nf metrics.NotFoundError
	var oe *output.Error
	switch {
	case errors.As(err, &nf), errors.Is(err, metrics.ErrUnknownView):
		return http.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, index.ErrUpdateInProgress):
		return http.StatusServiceUnavailable
	case errors.As(err, &oe) && oe.Code == output.Usage:
		return http.StatusBadRequest
	case errors.As(err, &oe) && oe.Code == output.Blocked:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// apiFailure is the status and the reported error of a raw error: the
// status comes from the raw error, before views.Error rewrites it; a
// deadline and an unclassified failure carry a fixed sentence.
func apiFailure(err error) (int, error) {
	status := httpStatus(err)
	reported := views.Error(err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status, &output.Error{Code: output.CodeOf(reported), Message: tooSlowSentence}
	case status == http.StatusInternalServerError:
		return status, &output.Error{Code: output.CodeOf(reported), Message: viewFailsSentence}
	}
	return status, reported
}

// apiError writes the JSON error envelope with status.
func apiError(w http.ResponseWriter, command string, status int, err error) {
	var buf bytes.Buffer
	if werr := output.WriteJSON(&buf, command, nil, err); werr != nil {
		refuse(w, http.StatusInternalServerError, viewFailsSentence)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
