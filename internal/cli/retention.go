package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
)

// recommendedRetention keeps transcripts ten years (decision 0041).
const recommendedRetention = 3650

type transcriptSummary struct {
	Count  int    `json:"count"`
	Bytes  int64  `json:"bytes"`
	Oldest string `json:"oldest,omitempty"` // YYYY-MM-DD
}

type retentionResult struct {
	Before       claudecode.Retention   `json:"before"`
	Transcripts  transcriptSummary      `json:"transcripts"`
	Target       int                    `json:"target"` // retention_days asked; 0 = --keep
	ConfigChange bool                   `json:"config_change"`
	Files        []claudecfg.FileResult `json:"files"`
	Run          string                 `json:"run,omitempty"`
	Notes        []string               `json:"notes,omitempty"`
	Plan         bool                   `json:"plan"`
}

func newClientRetentionCmd(e *env, configDir *string) *cobra.Command {
	var days int
	var keep, plan bool
	cmd := &cobra.Command{
		Use:   "retention [--days N | --keep]",
		Short: "Keep Claude Code transcripts long enough for backfill and rebuilds (default 3650 days)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			explicit := cmd.Flags().Changed("days")
			if explicit && days <= 0 {
				return &output.Error{Code: output.Usage, Message: "--days must be ≥ 1"}
			}
			if explicit && keep {
				return &output.Error{Code: output.Usage, Message: "--days and --keep exclude each other"}
			}
			res, err := clientRetention(*configDir, days, explicit, keep, plan)
			return e.report("client retention", res, func() string { return retentionText(res) }, err)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "keep transcripts exactly N days (may lower the current value)")
	cmd.Flags().BoolVar(&keep, "keep", false, "stop managing the retention; Claude Code's setting is left as it is")
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would change without writing")
	return cmd
}

func clientRetention(configDir string, days int, explicit, keep, plan bool) (retentionResult, error) {
	res := retentionResult{Plan: plan, Files: []claudecfg.FileResult{}}
	c, err := loadClientEnv(configDir)
	if err != nil {
		return res, err
	}
	if res.Before, err = claudecode.ReadRetention(c.loc, c.managed); err != nil {
		return res, &output.Error{Code: output.Blocked, Message: err.Error() + ": fix the file by hand first, backsight never overwrites it"}
	}
	tr, scanErr := claudecode.ScanTranscripts(c.loc.Projects)
	res.Transcripts = transcriptSummary{Count: tr.Count, Bytes: tr.Bytes}
	if !tr.Oldest.IsZero() {
		res.Transcripts.Oldest = tr.Oldest.UTC().Format("2006-01-02")
	}
	if scanErr != nil {
		res.Notes = append(res.Notes, "transcripts not counted: "+scanErr.Error())
	}
	switch {
	case keep:
		res.Target = 0
	case explicit:
		res.Target = days
	default:
		res.Target = recommendedRetention
	}
	if res.Before.ManagedDays != nil && !keep {
		res.Notes = append(res.Notes, fmt.Sprintf("a managed setting forces cleanupPeriodDays = %d: ask your administrator; nothing written", *res.Before.ManagedDays))
		return res, nil
	}

	// backsight's own config first: it holds the floor that client config
	// apply keeps afterwards.
	raw, sha, err := fsutil.ReadHashed(c.dirs.ConfigFile())
	if err != nil {
		return res, &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	newRaw := raw
	if sha == fsutil.Absent && res.Target > 0 {
		newRaw = []byte("schema_version = 1\n")
	}
	if newRaw, err = config.SetClientRetention(newRaw, res.Target); err != nil {
		return res, &output.Error{Code: output.Usage, Message: err.Error()}
	}
	if _, err := config.Parse(newRaw, c.dirs.ConfigFile(), c.dirs, ""); err != nil {
		return res, &output.Error{Code: output.Usage, Message: "the edited config would not load: " + err.Error()}
	}
	res.ConfigChange = string(newRaw) != string(raw)

	var edits []claudecfg.Edit
	switch {
	case keep:
	case explicit:
		edits = append(edits, claudecfg.Edit{Target: c.loc.Settings, Apply: setRetention(days)})
	default:
		edits = append(edits, claudecfg.Edit{Target: c.loc.Settings, Apply: retentionFloor(recommendedRetention)})
	}
	if res.Files, err = claudecfg.Plan(edits); err != nil || plan {
		return res, editError(err)
	}
	if err := recordBefore(c.dirs, "client retention", res.Files, c.loc.Settings); err != nil {
		return res, editError(err)
	}
	if res.ConfigChange {
		if err := fsutil.ReplaceIfUnchanged(c.dirs.ConfigFile(), newRaw, 0o600, sha); err != nil {
			return res, &output.Error{Code: output.Blocked, Message: err.Error()}
		}
	}
	res.Run, res.Files, err = c.runner("client retention").Apply(edits)
	if res.Files == nil {
		res.Files = []claudecfg.FileResult{}
	}
	if err != nil && res.ConfigChange {
		return res, partialError(err)
	}
	return res, editError(err)
}

func retentionText(r retentionResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Claude Code keeps transcripts %d days (%s); %d transcript(s), %.1f MB", r.Before.EffectiveDays, r.Before.Source,
		r.Transcripts.Count, float64(r.Transcripts.Bytes)/(1<<20))
	if r.Transcripts.Oldest != "" {
		b.WriteString(", oldest " + r.Transcripts.Oldest)
	}
	b.WriteString("\n")
	verb := map[bool]string{true: "would change", false: "changed"}[r.Plan]
	for _, f := range r.Files {
		for _, ch := range f.Changes {
			fmt.Fprintf(&b, "%s %s: %s: %s → %s\n", verb, f.Target, ch.Path, showValue(ch.Old), showValue(ch.New))
		}
	}
	if r.ConfigChange {
		if r.Target == 0 {
			fmt.Fprintf(&b, "%s backsight config: retention no longer managed\n", verb)
		} else {
			fmt.Fprintf(&b, "%s backsight config: retention_days = %d\n", verb, r.Target)
		}
	}
	for _, n := range r.Notes {
		b.WriteString("note: " + n + "\n")
	}
	if r.Run != "" {
		fmt.Fprintf(&b, "undo the file change with backsight client config restore %s\n", r.Run)
	}
	return b.String()
}
