package cli

import (
	"fmt"
	"io"

	"github.com/Barben360/backsight/internal/termstyle"
)

// The roles of the status words commands print (decision 0084 D5): the
// word stays, the color only helps reading it.
var statusRoles = map[string]termstyle.Role{
	"ok": termstyle.OK, "done": termstyle.OK, "completed": termstyle.OK, "stored": termstyle.OK,
	"recorded": termstyle.OK, "started": termstyle.OK, "up to date": termstyle.OK,
	"WARN": termstyle.Warn, "warn": termstyle.Warn, "warning": termstyle.Warn, "waiting": termstyle.Warn,
	"ERROR": termstyle.Fail, "fail": termstyle.Fail, "failed": termstyle.Fail, "refused": termstyle.Fail,
	"rejected": termstyle.Fail, "lost": termstyle.Fail,
	"planned": termstyle.Accent,
	"skipped": termstyle.Secondary, "kept": termstyle.Secondary, "none": termstyle.Secondary,
	"info": termstyle.Secondary, "note": termstyle.Secondary, "pending": termstyle.Secondary,
	"queued": termstyle.Secondary, "next": termstyle.Secondary,
}

// statusRole is word's role; false for a word without one.
func statusRole(word string) (termstyle.Role, bool) {
	r, ok := statusRoles[word]
	return r, ok
}

// statusCell is word painted by its role and padded to width: a word
// without a role is only padded.
func statusCell(st termstyle.Style, word string, width int) string {
	r, ok := statusRole(word)
	if !ok {
		return termstyle.Style{}.Cell(r, word, width)
	}
	return st.Cell(r, word, width)
}

// statusWord is word painted by its role, unpadded.
func statusWord(st termstyle.Style, word string) string { return statusCell(st, word, 0) }

// errorLine is the "backsight: <error>" line of stream w, its prefix red
// when w's color is on.
func errorLine(w io.Writer, err any) string {
	return termstyle.StyleOf(w).Paint(termstyle.Fail, "backsight:") + " " + fmt.Sprint(err) + "\n"
}
