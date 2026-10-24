package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

type whyResult struct {
	Session    string              `json:"session"`
	Injections []session.Injection `json:"injections"`
}

func newWhyCmd(e *env) *cobra.Command {
	var id string
	var last int
	cmd := &cobra.Command{
		Use:   "why",
		Short: "Show the last memories the hooks injected, with their scores, scopes and the rule that let them through",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(*cobra.Command, []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			store := session.Store{Dir: d.SessionsDir(), Locks: d.LocksDir()}
			if id == "" {
				id = os.Getenv("BACKSIGHT_SESSION")
			}
			if id == "" {
				if id, err = store.Latest(); err != nil {
					return blocked(err)
				}
			}
			res := whyResult{Session: id, Injections: []session.Injection{}}
			if id != "" {
				if !session.ValidID(id) {
					return &output.Error{Code: output.Usage, Message: "invalid session id " + id}
				}
				st, err := store.Load(id)
				if err != nil {
					return &output.Error{Code: output.Blocked, Message: "session state unreadable: " + err.Error()}
				}
				res.Injections = st.Injected
				if last > 0 && len(res.Injections) > last {
					res.Injections = res.Injections[len(res.Injections)-last:]
				}
			}
			return e.emit("why", res, func() string { return whyText(res) })
		},
	}
	cmd.Flags().StringVar(&id, "session", "", "session id (default: $BACKSIGHT_SESSION, else the latest session)")
	cmd.Flags().IntVar(&last, "last", 5, "injections shown")
	return cmd
}

func whyText(r whyResult) string {
	if len(r.Injections) == 0 {
		return "no injection recorded for this session\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "session %s\n", r.Session)
	for _, in := range r.Injections {
		where := fmt.Sprintf("prompt %d", in.Prompt)
		if in.Prompt == 0 {
			where = "startup briefing"
		}
		fmt.Fprintf(&b, "- %s, rule %s: [%s] %s (%s, scope %s, %.2f)\n", where, in.Rule, in.Project, in.Text, in.ID, in.Scope, in.Score)
	}
	return b.String()
}
