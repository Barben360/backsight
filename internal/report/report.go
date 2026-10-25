// Package report builds `backsight report` (SPEC §15, decision 0002): a
// redacted diagnostic, a search link for existing issues and a pre-filled
// new-issue link. Nothing here sends anything; the user reads the result
// and decides.
package report

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
)

// Repo is where issues go. Links are built from it, never from a git
// remote: the binary may run anywhere, and a fork's remote is not where
// backsight's maintainers read.
const Repo = "Barben360/backsight"

const repoURL = "https://github.com/" + Repo

// MaxURL bounds the new-issue link. GitHub and browsers refuse links of
// about 8 KB; the rest of the diagnostic goes in the block the user pastes.
const MaxURL = 6000

// Versions of everything a bug may depend on.
type Versions struct {
	Binary            string `json:"binary"`
	Commit            string `json:"commit"`
	Date              string `json:"date"`
	Go                string `json:"go"`
	Plugin            string `json:"plugin"`
	HindsightPin      string `json:"hindsight_pin"`
	HindsightDeployed string `json:"hindsight_deployed"`
	ClaudeCode        string `json:"claude_code"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	OSRelease         string `json:"os_release,omitempty"`
}

// Server is what the diagnostic says about the server: never its URL's
// host when it is not loopback, never the bank name or the key.
type Server struct {
	Source   string `json:"source"` // config | reference
	Scheme   string `json:"scheme"`
	Loopback bool   `json:"loopback"`
}

// Doctor is doctor's JSON report with the server reduced to Server.
type Doctor struct {
	Checks   []doctor.Check `json:"checks"`
	Server   *Server        `json:"server,omitempty"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
}

// Counts are local numbers only (no server call).
type Counts struct {
	SessionsSent        int `json:"sessions_sent"`
	SessionsTracked     int `json:"sessions_tracked"`
	SessionsQuarantined int `json:"sessions_quarantined"`
	NotesQueued         int `json:"notes_queued"`
	NotesQuarantined    int `json:"notes_quarantined"`
	// ErrorClasses counts the telemetry errors of the last Days days by class.
	ErrorClasses map[string]int `json:"error_classes"`
	Days         int            `json:"days"`
}

// ErrorEvent is one recent backsight error from the telemetry.
type ErrorEvent struct {
	TS      time.Time `json:"ts"`
	Class   string    `json:"class"`
	Message string    `json:"message,omitempty"`
}

// Diagnostic is everything the report shows.
type Diagnostic struct {
	GeneratedAt  time.Time    `json:"generated_at"`
	Versions     Versions     `json:"versions"`
	Doctor       Doctor       `json:"doctor"`
	Counts       Counts       `json:"counts"`
	RecentErrors []ErrorEvent `json:"recent_errors"`
}

// Report is the command's result.
type Report struct {
	// Sent is always false: backsight never submits anything (0002).
	Sent bool `json:"sent"`
	// Class is the error class the issue is about ("" on a healthy machine).
	Class string `json:"class,omitempty"`
	// SearchURL lists the existing issues with the same class.
	SearchURL string `json:"search_url,omitempty"`
	Title     string `json:"title"`
	// IssueURL opens a new issue with the title and a summary filled in.
	IssueURL string `json:"issue_url"`
	// Omitted counts the summary lines left out of IssueURL to keep it short.
	Omitted int `json:"omitted"`
	// Paste is the full diagnostic, as a block the user pastes into the issue.
	Paste      string     `json:"paste"`
	Diagnostic Diagnostic `json:"diagnostic"`
}

// Build scrubs d and turns it into the report and its links.
func Build(d Diagnostic, s *Scrubber) (Report, error) {
	d, err := s.Diagnostic(d)
	if err != nil {
		return Report{}, err
	}
	r := Report{Diagnostic: d, Class: Class(d)}
	if r.Class != "" {
		r.SearchURL = SearchURL(r.Class)
	}
	r.Title = title(d, r.Class)
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return Report{}, err
	}
	r.Paste = "<details><summary>backsight report " + d.Versions.Binary + " (redacted)</summary>\n\n```json\n" + string(raw) + "\n```\n\n</details>\n"
	r.IssueURL, r.Omitted = issueURL(r.Title, d, r.Class)
	return r, nil
}

// Class names what the issue is about, the same way for every user so the
// search finds duplicates: the first doctor error, else the most frequent
// telemetry error class, else the first doctor warning.
func Class(d Diagnostic) string {
	for _, c := range d.Doctor.Checks {
		if c.Status == doctor.Error {
			return "doctor/" + c.ID + "/error"
		}
	}
	best, n := "", 0
	for k, v := range d.Counts.ErrorClasses {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	if best != "" {
		return best
	}
	for _, c := range d.Doctor.Checks {
		if c.Status == doctor.Warn {
			return "doctor/" + c.ID + "/warn"
		}
	}
	return ""
}

// SearchURL lists the issues, open or closed, whose text holds class.
func SearchURL(class string) string {
	return repoURL + "/issues?q=" + url.QueryEscape(`is:issue "`+class+`"`)
}

func title(d Diagnostic, class string) string {
	if class == "" {
		return "report: backsight " + d.Versions.Binary
	}
	t := "report: " + class
	for _, c := range d.Doctor.Checks {
		if "doctor/"+c.ID+"/"+string(c.Status) == class {
			t += ": " + c.Summary
			break
		}
	}
	return truncate(t, 120)
}

const pastePlaceholder = "<!-- paste here the block `backsight report` printed -->"

// issueURL fills the summary in line by line while the link stays under
// MaxURL; the lines left out are in the pasted block anyway.
func issueURL(title string, d Diagnostic, class string) (string, int) {
	v := d.Versions
	head := []string{
		"<!-- Pre-filled by `backsight report`. Nothing was sent: read it, edit it, then submit or close the tab. -->",
		"",
		"**What happened**",
		"",
		"<!-- what you did, what you expected, what you saw -->",
		"",
	}
	if class != "" {
		head = append(head, "**Error class**: `"+class+"`", "")
	}
	head = append(head,
		fmt.Sprintf("**Versions**: backsight %s (%s), plugin %s, Hindsight pin %s, deployed %s, Claude Code %s, %s/%s %s",
			v.Binary, v.Commit, orNone(v.Plugin), orNone(v.HindsightPin), orNone(v.HindsightDeployed), orNone(v.ClaudeCode), v.OS, v.Arch, v.OSRelease),
		"",
		fmt.Sprintf("**Doctor**: %d error(s), %d warning(s)", d.Doctor.Errors, d.Doctor.Warnings))
	tail := []string{"", "**Full diagnostic**", "", pastePlaceholder}

	var optional []string
	for _, c := range d.Doctor.Checks {
		if c.Status == doctor.Error || c.Status == doctor.Warn {
			optional = append(optional, fmt.Sprintf("- %s `%s`: %s", c.Status, c.ID, truncate(c.Summary, 200)))
		}
	}
	c := d.Counts
	optional = append(optional, "", fmt.Sprintf("**Counts**: %d session(s) sent, %d tracked, %d quarantined; %d note(s) queued, %d quarantined",
		c.SessionsSent, c.SessionsTracked, c.SessionsQuarantined, c.NotesQueued, c.NotesQuarantined))
	if len(c.ErrorClasses) > 0 {
		keys := make([]string, 0, len(c.ErrorClasses))
		for k := range c.ErrorClasses {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if c.ErrorClasses[keys[i]] != c.ErrorClasses[keys[j]] {
				return c.ErrorClasses[keys[i]] > c.ErrorClasses[keys[j]]
			}
			return keys[i] < keys[j]
		})
		optional = append(optional, "", fmt.Sprintf("**Errors in the last %d days**:", c.Days))
		for _, k := range keys {
			optional = append(optional, fmt.Sprintf("- `%s` × %d", k, c.ErrorClasses[k]))
		}
	}
	if len(d.RecentErrors) > 0 {
		optional = append(optional, "", "**Recent errors**:")
		for _, e := range d.RecentErrors {
			line := fmt.Sprintf("- %s `%s`", e.TS.UTC().Format("2006-01-02 15:04Z"), e.Class)
			if e.Message != "" {
				line += ": " + truncate(e.Message, 200)
			}
			optional = append(optional, line)
		}
	}

	link := func(lines []string) string {
		return repoURL + "/issues/new?title=" + url.QueryEscape(title) + "&body=" + url.QueryEscape(strings.Join(lines, "\n"))
	}
	const note = "_Some lines were left out to keep this link short: they are in the pasted diagnostic._"
	body := append([]string{}, head...)
	omitted := 0
	for _, l := range optional {
		try := append(append(append([]string{}, body...), l), "", note)
		try = append(try, tail...)
		if omitted == 0 && len(link(try)) <= MaxURL {
			body = append(body, l)
			continue
		}
		if strings.TrimSpace(l) != "" && !strings.HasSuffix(l, ":") {
			omitted++
		}
	}
	if omitted > 0 {
		body = append(body, "", note)
	}
	if l := link(append(body, tail...)); len(l) <= MaxURL {
		return l, omitted
	}
	// Even the head is too long (huge version strings): the bare minimum.
	return link(append(head[:6:6], tail...)), omitted + len(head) - 6
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// truncate cuts s to n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
